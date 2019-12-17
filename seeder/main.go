package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gregdel/pushover"
	"github.com/korovkin/limiter"
	"github.com/lib/pq"
)

const (
	connLimit   = 16
	routines    = 8
	doneLog     = "done.log"
	errLog      = "done.err"
	testTar     = "../tests/test_data.tar.gz"
	upsertQuery = `
		WITH ins1 AS (
			INSERT INTO usernames(username) VALUES ($1)
			ON CONFLICT (username) DO UPDATE SET username=EXCLUDED.username
			RETURNING id AS username_id
		)
		, ins2 AS (
			INSERT INTO passwords(password) VALUES ($2)
			ON CONFLICT (password) DO UPDATE SET password=EXCLUDED.password
			RETURNING id AS pass_id
		)
		, ins3 AS (
			INSERT INTO domains(domain) VALUES ($3)
			ON CONFLICT (domain) DO UPDATE SET domain=EXCLUDED.domain
			RETURNING id AS domain_id
		)

		INSERT INTO records (username_id, password_id, domain_id)
		VALUES (
			(select username_id from ins1), 
			(select pass_id from ins2), 
			(select domain_id from ins3) 
		)
		ON CONFLICT (username_id, password_id, domain_id) DO NOTHING
`)

var (
	finished map[string]bool
)

// sets up the progress map so we can skip files that have already been processed
func init() {
	progressFile, err := os.Open(doneLog)
	finished = make(map[string]bool)
	if err != nil {
		switch err {
		case (err).(*os.PathError):
			progressFile, err = os.Create(doneLog)
		default:
			panic(err)
		}
	}
	defer progressFile.Close()

	fileScanner := bufio.NewScanner(progressFile)
	for fileScanner.Scan() {
		f := fileScanner.Text()
		finished[f] = true
	}
}

func main() {
	f, err := os.OpenFile(errLog, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	connStr := os.Getenv("PG_CONN")
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(connLimit)
	db.SetMaxIdleConns(connLimit)
	db.SetConnMaxLifetime(connLimit * time.Second)

	// initiate count for this connection
	updateCount(db)

	var tarGzPath string
	if os.Getenv("TEST") != "" {
		tarGzPath = testTar
	} else {
		tarGzPath = os.Args[1]
	}

	tarGz, err := os.Open(tarGzPath)
	if err != nil {
		log.Fatal(err)
	}
	defer tarGz.Close()

	gzf, err := gzip.NewReader(tarGz)
	if err != nil {
		log.Fatal(err)
	}

	tarReader := tar.NewReader(gzf)

	var counter int
	go alert("Starting TARGZ: " + tarGzPath)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("ExtractTarGz: Next() failed: %s", err.Error())
		}

		if header.Typeflag == tar.TypeReg {
			if alreadyRan(header.Name) {
				fmt.Printf("Skipping: %s\n", header.Name)
				continue
			}

			if !strings.HasSuffix(header.Name, ".txt") {
				fmt.Printf("Skipping: %s\n", header.Name)
				continue
			}

			var wg sync.WaitGroup
			lineCh := make(chan string, routines*2)

			// process lines in the background as they come in to the lineCh channel
			// processing has not yet begun, but this 'listener' needs to be set up
			// first
			fmt.Println("Starting file: " + header.Name)
			limit := limiter.NewConcurrencyLimiter(routines)
			go func(
				wgi *sync.WaitGroup,
				dbi *sql.DB,
				ch chan string,
				lim *limiter.ConcurrencyLimiter,
			) {
				for _line := range ch {
					line := _line
					lim.Execute(func() {
						processAndSave(wgi, dbi, line)
					})
				}
			}(&wg, db, lineCh, limit)

			// iterate through the lines in the file, adding each to the workgroup
			// before dispatching the line to the processing listener
			lineReader := bufio.NewScanner(tarReader)
			precount := count(db)
			counter = 0
			for lineReader.Scan() {
				lineCh <- lineReader.Text()
				counter++
				wg.Add(1)
			}

			wg.Wait()
			postcount := count(db)
			skipped := counter - (postcount - precount)
			go reportStats(db, header.Name, counter, skipped, postcount, false)
			markDone(header.Name)
		}
	}
	go reportStats(db, tarGzPath, 0, 0, count(db), true)
}

// helper for making queries that return a single int
func intQuery(db *sql.DB, query string) (int, error) {
	var out int
	rows, err := db.Query(query)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		rows.Scan(&out)
	}
	return out, err
}

func count(db *sql.DB) int {
	num, _ := intQuery(db, "SELECT nextval('total')")
	return num
}

// send stats to a pushover acccount. called concurrently since our
// data-processing doesn't rely an anything in here
func reportStats(db *sql.DB, filename string, counter, skipped, total int, pb bool) {
	msg := fmt.Sprintf(
		"Finished processing: %s\nProcessed: %d\nSkipped: %d\nTotal: %d\n",
		filename,
		counter,
		skipped,
		total,
	)

	fmt.Println(msg)
	if pb {
		alert(msg)
	}
}

// takes a raw line, converts it into data the DB would want and attempts
// to persist the record
func processAndSave(wg *sync.WaitGroup, db *sql.DB, lineText string) {
	defer wg.Done()

	user, domain, password := parse(lineText)
	err := upsert(db, user, domain, password)

	if err != nil {
		pqErr := (err).(*pq.Error)
		switch pqErr.Code.Name() {
		case "unique_violation":
			// do nothing, there are a lot of these
			// especially when restarting import jobs
		case "character_not_in_repertoire":
			go log.Printf(
				"ENC line=%s|username=%s|domain=%s|password=%s|msg=%s",
				lineText,
				user,
				domain,
				password,
				pqErr.Message,
			)
		default:
			log.Printf("ERR %s - %s", lineText, pqErr.Message)
		}
	}
}

// attempt to commit data in a transaction. a new Record depends on
// a user, password, and domain existing. record creation should be
// idempotent given the ON CONFLICT clause in the query. #upsert
// returns a pq.Error
func upsert(db *sql.DB, user, domain, password string) error {
	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("Failed starting database transaction: %s", err.Error())
		return err
	}

	preparedQuery, err := tx.Prepare(upsertQuery)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer preparedQuery.Close()

	_, err = preparedQuery.Exec(user, password, domain)
	if err != nil {
		tx.Rollback()
		return err
	}

	updateCount(db)
	return tx.Commit()
}

func updateCount(db *sql.DB) {
	intQuery(db, "SELECT nextval('total')")
}

// contains the logic for breaking a line into desired username
// password and email domain. currently accounts for the password
// delimiter being both a : and a ;
func parse(line string) (user, domain, password string) {
	user, domain, password = "nil", "nil", "nil"

	userAndRest := strings.SplitN(line, "@", 2)
	if len(userAndRest) != 2 {
		return user, domain, password
	}

	if len(userAndRest) == 2 {
		user = strings.ToLower(userAndRest[0])
	}

	domainAndPass := strings.SplitN(userAndRest[1], ":", 2)
	if len(domainAndPass) == 2 {
		domain = strings.ToLower(domainAndPass[0])
		password = domainAndPass[1]
		return
	}

	domainAndPass = strings.SplitN(userAndRest[1], ";", 2)
	if len(domainAndPass) == 2 {
		domain = strings.ToLower(domainAndPass[0])
		password = domainAndPass[1]
		return
	}
	domain = domainAndPass[0]
	return
}

// send text to pushover account // moblie phone
func alert(text string) {
	app := pushover.New(os.Getenv("PO_API"))
	me := pushover.NewRecipient(os.Getenv("PO_USR"))
	msg := pushover.NewMessage(text)
	app.SendMessage(msg, me)
}

// commits text to a file. primarily used to append filenames that
// have been processed already
func logger(file string) {
	f, err := os.OpenFile(doneLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer f.Close()
	if _, err := f.WriteString(file + "\n"); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// updated the in-memory progress map and commits the filename
// to our progress log
func markDone(file string) {
	finished[file] = true
	logger(file)
}

// duh
func alreadyRan(file string) bool {
	return finished[file]
}
