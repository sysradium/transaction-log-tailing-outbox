package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
)

func main() {
	logger := slog.Default()

	conn, err := pgconn.Connect(context.Background(), os.Getenv("PGLOGREPL_DEMO_CONN_STRING"))
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}
	defer conn.Close(context.Background())

	/*
		result := conn.Exec(context.Background(), "DROP PUBLICATION IF EXISTS pglogrepl_demo;")
		_, err = result.ReadAll()
		if err != nil {
			log.Fatalln("drop publication if exists error", err)
		}
	*/

	result := conn.Exec(context.Background(), "CREATE PUBLICATION pglogrepl_demo FOR ALL TABLES;")
	if _, err := result.ReadAll(); err != nil {
		if err.(*pgconn.PgError).Code != "42710" {
			log.Fatalln("create publication error", err)
		}
	}
	log.Println("create publication pglogrepl_demo")

	slotName := "pglogrepl_demo"
	pluginArguments := []string{
		"proto_version '2'",
		fmt.Sprintf("publication_names '%v'", slotName),
		"messages 'true'",
		"streaming 'true'",
	}

	_, err = pglogrepl.CreateReplicationSlot(context.Background(), conn, slotName, "pgoutput", pglogrepl.CreateReplicationSlotOptions{Temporary: false})
	if err != nil {
		if err.(*pgconn.PgError).Code != "42710" {
			log.Fatalln("CreateReplicationSlot failed:", err)
		}
	}
	log.Println("Created temporary replication slot:", slotName)

	err = pglogrepl.StartReplication(context.Background(), conn, slotName, pglogrepl.LSN(0), pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments})
	if err != nil {
		log.Fatalln("StartReplication failed:", err)
	}
	log.Println("Logical replication started on slot", slotName)

	clientXLogPos := pglogrepl.LSN(0)
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)
	relationsV2 := map[uint32]*pglogrepl.RelationMessageV2{}
	typeMap := pgtype.NewMap()

	// whenever we get StreamStartMessage we set inStream to true and then pass it to DecodeV2 function
	// on StreamStopMessage we set it back to false
	inStream := false

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), conn, pglogrepl.StandbyStatusUpdate{
				WALWritePosition: clientXLogPos + 1,
				WALFlushPosition: 0,
				WALApplyPosition: 0,
				ClientTime:       time.Time{},
				ReplyRequested:   false,
			})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			log.Printf("Sent Standby status message at %s\n", clientXLogPos.String())
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := conn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Fatalf("received Postgres WAL error: %+v", errMsg)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			logger.Debug("Received unexpected message", "message", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			logger.Debug("Primary Keepalive Message =>", "ServerWALEnd:", pkm.ServerWALEnd, "ServerTime:", pkm.ServerTime, "ReplyRequested:", pkm.ReplyRequested)
			if pkm.ServerWALEnd > clientXLogPos {
				clientXLogPos = pkm.ServerWALEnd
			}
			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}

			logger.Debug("XLogData => WALStart %s ServerWALEnd %s ServerTime %s WALData:\n", xld.WALStart, xld.ServerWALEnd, xld.ServerTime)
			inStream, err = processV2(xld.WALData, relationsV2, typeMap, inStream)
			if err != nil {
				log.Fatalf("unable to process message: %w", err)
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}
		}
	}
}

func processV2(walData []byte, relations map[uint32]*pglogrepl.RelationMessageV2, typeMap *pgtype.Map, inStream bool) (bool, error) {
	logicalMsg, err := pglogrepl.ParseV2(walData, inStream)
	if err != nil {
		return inStream, fmt.Errorf("parse logical replication message: %w", err)
	}

	log.Printf("Receive a logical replication message: %s", logicalMsg.Type())

	switch logicalMsg := logicalMsg.(type) {
	case *pglogrepl.RelationMessageV2:
		relations[logicalMsg.RelationID] = logicalMsg
	case *pglogrepl.BeginMessage:
	case *pglogrepl.CommitMessage:
	case *pglogrepl.InsertMessageV2:
		rel, ok := relations[logicalMsg.RelationID]
		if !ok {
			log.Fatalf("unknown relation ID %d", logicalMsg.RelationID)
		}
		values := map[string]interface{}{}
		for idx, col := range logicalMsg.Tuple.Columns {
			colName := rel.Columns[idx].Name
			switch col.DataType {
			case 'n': // null
				values[colName] = nil
			case 'u': // unchanged toast
				// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
			case 't': // text
				val, err := decodeTextColumnData(typeMap, col.Data, rel.Columns[idx].DataType)
				if err != nil {
					log.Fatalln("error decoding column data:", err)
				}
				values[colName] = val
			}
		}
		log.Printf("INSERT INTO %s.%s: %v", rel.Namespace, rel.RelationName, values)

	case *pglogrepl.StreamStartMessageV2:
		return true, nil
	case *pglogrepl.StreamStopMessageV2:
		return false, nil
	case *pglogrepl.UpdateMessageV2:
	case *pglogrepl.DeleteMessageV2:
	case *pglogrepl.TruncateMessageV2:
	case *pglogrepl.TypeMessageV2:
	case *pglogrepl.OriginMessage:
	case *pglogrepl.LogicalDecodingMessageV2:
	case *pglogrepl.StreamCommitMessageV2:
	case *pglogrepl.StreamAbortMessageV2:
	}

	return inStream, nil
}

func decodeTextColumnData(mi *pgtype.Map, data []byte, dataType uint32) (interface{}, error) {
	if dt, ok := mi.TypeForOID(dataType); ok {
		return dt.Codec.DecodeValue(mi, dataType, pgtype.TextFormatCode, data)
	}
	return string(data), nil
}
