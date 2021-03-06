package main
import (
	"fmt"
	"encoding/json"
	"github.com/ziutek/mymysql/mysql"
	"github.com/streadway/amqp"
	"log"
	"strconv"
	"github.com/ziutek/mymysql/autorc"
	_ "github.com/ziutek/mymysql/native"
	"time"
)

// TODO: Try for at least 3 times before discarding a message
// DONE: Implement kill channel for the goroutine
func gcm_error_processor_token_update(config Configuration, conn *amqp.Connection, GcmTokenUpdateQueueName string,
		ch_custom_err chan []byte, logger *log.Logger, killTokenUpd, killTokenUpdAck chan int, gq GcmQueue) {
	// Connect to a database
	db := autorc.New("tcp", "", config.Db.DbHost+":"+strconv.Itoa(config.Db.DbPort), config.Db.DbUser, config.Db.DbPassword, config.Db.DbDatabase)

	var upd autorc.Stmt
	err := db.PrepareOnce(&upd, gq.Queries.TokenUpdate)
	if err != nil {
		failOnError(err, "Could not create prepared statement")
	}
	//	upd, _ := db.Prepare("UPDATE bobble_user_gcm SET gcm_id = ?, gcm_status = 1 WHERE gcm_id = ?")

	// Create new channel for Token update
	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	err = ch.Qos(
		1,     // prefetch count
		0,     // prefetch size
		false, // global
	)
	failOnError(err, "Failed to set QoS")

	msgsTokenUpdate, err := ch.Consume(
		GcmTokenUpdateQueueName, // queue
		"",     // consumer
		false,  // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	failOnError(err, "Failed to register a consumer")

	i := 0
	payloads := make([]GcmTokenUpdateMsg, config.Db.TransactionMinCount.StatusInactive)
	for {
		select {
		case d, ok := <-msgsTokenUpdate:
			if !ok {
				continue
			}
			olog(fmt.Sprintf("Token Update Received a message: %s", d.Body), config.DebugMode)

			payload  := GcmTokenUpdateMsg{}
			err := json.Unmarshal(d.Body, &payload)

			if err != nil {
				logger.Printf("Unmarshal error for Token Update MQ message data = %s",d.Body)
				olog(fmt.Sprintf("Unmarshal error for Token Update MQ message data = %s",d.Body), config.DebugMode)
			} else {
				payloads[i] = payload
				i++

				if i == config.Db.TransactionMinCount.TokenUpdate {
					i = 0
					err := db.Begin(func(tr mysql.Transaction, args ...interface{}) error {
						for _, v := range payloads {
							_, err := tr.Do(upd.Raw).Run(v.NewToken, v.OldToken)
							if err != nil {
								return err
							}
						}
						return tr.Commit()
					})
					t := time.Now()
					ts := t.Format(time.RFC3339)
					if err != nil {
						// Error while updating db
						olog("Database Transaction Error ErrTokenUpdateTransaction", config.DebugMode)

						errInfo := make(map[string]interface{})
						errInfo["error"] = err.Error()
						errInfo["payloads"] = payloads
						errLog := DbLog{TimeStamp:ts, Type:StatusErrTokenUpdateTransaction, Data:errInfo}
						errLogByte, err := json.Marshal(errLog)
						if err == nil {
							ch_custom_err <- errLogByte
						} else {
							logger.Printf("Marshal error for ErrTokenUpdateTransaction")
						}
					} else {
						// Db successfuly updated
						olog("Database Transaction Success StatusSuccessTokenUpdateTransaction", config.DebugMode)
						errLog := DbLog{TimeStamp:ts, Type:StatusSuccessTokenUpdateTransaction, Data:payloads}

						errLogByte, err := json.Marshal(errLog)
						if err == nil {
							ch_custom_err <- errLogByte
						} else {
							logger.Printf("Marshal error for StatusSuccessTokenUpdateTransaction")
						}

						// For for specified time before running next query
						time.Sleep(time.Duration(config.Db.WaitTimeMs.TokenUpdate) * time.Millisecond)
					}
				}
			}
			// Acknowledge to MQ that work has been processed successfully
			d.Ack(false)
		case ack := <-killTokenUpd:
			olog("Killing GCM token update goroutine", config.DebugMode)
			// Write to database and exit from goroutine
			if i > 0 {
				i = 0
				err := db.Begin(func(tr mysql.Transaction, args ...interface{}) error {
					for _, v := range payloads {
						_, err := tr.Do(upd.Raw).Run(v.NewToken, v.OldToken)
						if err != nil {
							return err
						}
					}
					return tr.Commit()
				})
				if err != nil {
					olog("Database Transaction Error while exiting + StatusErrTokenUpdateTransaction", config.DebugMode)
					t := time.Now()
					ts := t.Format(time.RFC3339)
					errInfo := make(map[string]interface{})
					errInfo["error"] = err.Error()
					errInfo["payloads"] = payloads
					errLog := DbLog{TimeStamp:ts, Type:StatusErrTokenUpdateTransaction, Data:errInfo}
					errLogByte, err := json.Marshal(errLog)
					if err == nil {
						ch_custom_err <- errLogByte
					} else {
						logger.Printf("Marshal error for ErrTokenUpdateTransaction while quiting")
					}
				}
			}
			if ack == NeedAck {
				killTokenUpdAck<- 1
			}

			olog("GCM token update goroutine killed", config.DebugMode)
			// Exit from goroutine
			return
		}
	}
}
