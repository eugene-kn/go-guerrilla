package backends

import (
	"bytes"
	"database/sql"
	"errors"
	"io/ioutil"
	netmail "net/mail"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
	"github.com/go-sql-driver/mysql"
)

// ----------------------------------------------------------------------------------
// Processor Name: guid_filter
// ----------------------------------------------------------------------------------
// Description   : Extracts a guid from the email subject and looks it up
//               : in the "pings" table, if the guid is not found returns an error
//               : and thus prevents the next processor in the chain (MySQL one)
//               : from storing the email in the database. If the guid is found
//               : then it calls the next processor allowing the email to be saved.
// ----------------------------------------------------------------------------------
// Config Options: mail_table string - mysql table name
//               : mysql_db string - mysql database name
//               : mysql_host string - mysql host name, eg. 127.0.0.1
//               : mysql_pass string - mysql password
//               : mysql_user string - mysql username
//               : primary_mail_host string - primary host name
// --------------:-------------------------------------------------------------------
// Input         : e.Subject - generated by by ParseHeader() processor
// ----------------------------------------------------------------------------------
// Output        : Sets e.QueuedId with the first item fromHashes[0]
// ----------------------------------------------------------------------------------
func init() {
	processors["guidfilter"] = func() Decorator {
		return GUIDFilter()
	}
}

type GUIDFilterProcessorConfig struct {
	GUIDFilterLookupTable string `json:"guid_filter_lookup_table"`
	GUIDFilterLookupField string `json:"guid_filter_lookup_field"`
	MysqlDB               string `json:"mysql_db"`
	MysqlHost             string `json:"mysql_host"`
	MysqlPass             string `json:"mysql_pass"`
	MysqlUser             string `json:"mysql_user"`
}

type GUIDFilterProcessor struct {
	cache  stmtCache
	config *GUIDFilterProcessorConfig
}

func (p *GUIDFilterProcessor) connect(config *GUIDFilterProcessorConfig) (*sql.DB, error) {
	var db *sql.DB
	var err error
	conf := mysql.Config{
		User:         config.MysqlUser,
		Passwd:       config.MysqlPass,
		DBName:       config.MysqlDB,
		Net:          "tcp",
		Addr:         config.MysqlHost,
		ReadTimeout:  procMySQLReadTimeout,
		WriteTimeout: procMySQLWriteTimeout,
		Params:       map[string]string{"collation": "utf8_general_ci"},
	}
	if db, err = sql.Open("mysql", conf.FormatDSN()); err != nil {
		Log().Error("cannot open mysql", err)
		return nil, err
	}
	// do we have permission to access the table?
	_, err = db.Query("SELECT * FROM " + p.config.GUIDFilterLookupTable + " LIMIT 1")
	if err != nil {
		//Log().Error("cannot select table", err)
		return nil, err
	}
	Log().Info("connected to mysql on tcp ", config.MysqlHost)
	return db, err
}

func GUIDFilter() Decorator {
	var config *GUIDFilterProcessorConfig
	var db *sql.DB
	filter := &GUIDFilterProcessor{}

	// open the database connection (it will also check if we can select the table)
	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		Log().Info("Initializing GUIDFIlter processor...")
		configType := BaseConfig(&GUIDFilterProcessorConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}
		config = bcfg.(*GUIDFilterProcessorConfig)
		filter.config = config
		db, err = filter.connect(config)
		if err != nil {
			return err
		}
		return nil
	}))

	return func(p Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {

			if task == TaskSaveMail {
				m := regexp.MustCompile("guid:\\s*?(\\S+)\\s*?$").FindStringSubmatch(e.Subject)

				if m == nil {
					Log().Warn("Could not extract GUID from the subject")
					e.Values["ignore"] = true
				} else {
					guid := m[1]
					var guidFound string

					err := db.QueryRow("SELECT "+filter.config.GUIDFilterLookupField+
						" FROM "+filter.config.GUIDFilterLookupTable+
						" WHERE guid=? AND seen=0", guid).Scan(&guidFound)

					if err == sql.ErrNoRows {
						Log().Infof("GUID %s not found or it was already seen", guid)
						e.Values["ignore"] = true
					} else {
						if err != nil {
							Log().Errorf("Could not lookup GUID - %s", err.Error())
							e.Values["ignore"] = true
						}
					}

					if _, ok := e.Values["ignore"]; !ok {
						times := extractReceivedTimes([]byte(e.String()))
						delay := calculateDelay(times)

						stmt, err := db.Prepare("UPDATE " + filter.config.GUIDFilterLookupTable + " SET time_taken=?, header=?, body=?, received_time=?, seen=? WHERE guid=?")

						if err != nil {
							Log().WithError(err).Error("Could not prepare update statement")
						} else {
							header, body, err := parseHeaderAndBody(e.String())

							if err != nil {
								Log().WithError(err).Error("Could not parse header and body of email")
							}

							_, err = stmt.Exec(delay, header, body, time.Now(), 1, guid)

							if err != nil {
								Log().WithError(err).Error("Could not update delay")
							} else {
								Log().Infof("Updated delay (%ds) for GUID %s", delay, guid)
							}
						}
					}
				}
			}

			return p.Process(e, task)
		})
	}
}

type timestamps []time.Time

func (p timestamps) Len() int {
	return len(p)
}

func (p timestamps) Less(i, j int) bool {
	return p[i].Before(p[j])
}

func (p timestamps) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func parseRFC1123ZTime(s string) (time.Time, error) {
	m := regexp.MustCompile(`.*([A-Za-z_]{3}, \d+ [A-Za-z_]+ \d+ \d+:\d+:\d+ [-+]?\d+).*`).FindStringSubmatch(s)

	if m == nil {
		return time.Now(), errors.New("Could not find RFC1123Z time")
	}

	return netmail.ParseDate(m[1])
}

func extractReceivedTimes(message []byte) (times timestamps) {
	msg, err := netmail.ReadMessage(bytes.NewReader(message))

	if err != nil {
		return
	}

	rcvdHdrs, ok := msg.Header["Received"]

	if !ok {
		return
	}

	if len(rcvdHdrs) == 0 {
		return
	}

	for _, r := range rcvdHdrs {
		t, err := parseRFC1123ZTime(r)

		if err != nil {
			continue
		}

		times = append(times, t)
	}

	return times
}

func calculateDelay(times timestamps) (delay int) {
	if times == nil {
		return
	}

	if len(times) < 2 {
		return
	}

	sort.Sort(times)
	first := times[0]
	last := times[len(times)-1]
	return int(last.Sub(first).Seconds())
}

func parseHeaderAndBody(message string) (header, body string, err error) {
	m, err := netmail.ReadMessage(strings.NewReader(message))

	if err != nil {
		return
	}

	bodyBytes, err := ioutil.ReadAll(m.Body)

	if err != nil {
		return
	}

	body = string(bodyBytes)
	matches := regexp.MustCompile("(?s)^(.+?)\n\n").FindStringSubmatch(message)

	if matches != nil && len(matches) >= 2 {
		header = matches[1]
	}

	return
}
