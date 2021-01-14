package main

// Copyright 2021 Blacksun Research Labs

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// 	http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	flags "github.com/jessevdk/go-flags"
	_ "github.com/mattn/go-sqlite3" // Sqlite
)

// ID is a single paste ID from the Daily API
type ID struct {
	ID   string `json:"id"`
	Tags string `json:"tags"`
	Date int    `json:"date"`
}

// Pastes holds an array of paste IDs
type Pastes struct {
	Data []ID `json:"data"`
}

type postBody struct {
	Body Pastes `json:"body"`
}

type options struct {
	Interval int `short:"i" long:"interval" description:"Time in minutes to wait before checking feeds" default:"5"`
}

var hostString string
var opts options
var parser = flags.NewParser(&opts, flags.Default)

func makeTables(db *sql.DB) error {
	log.Println("Creating `pastes` table if it does not exist")

	stmt, err := db.Prepare(
		"CREATE TABLE IF NOT EXISTS pastes (id INTEGER PRIMARY KEY, paste_ID VARCHAR(255) NOT NULL UNIQUE)",
	)
	if err != nil {
		log.Printf("failed to prepare create table statement for `pastes` table: %v ", err)
	}
	defer stmt.Close()

	tx, err := db.Begin()
	if err != nil {
		log.Printf("failed to begin transaction: %v", err)
	}

	_, err = tx.Stmt(stmt).Exec()
	if err != nil {
		log.Fatalf("failed to create `pastes` table: %v", err)
		tx.Rollback()
	}
	tx.Commit()
	if err != nil {
		return err
	}

	return nil
}

func open() (db *sql.DB, err error) {
	db, err = sql.Open("sqlite3", "./pastes.db")
	if err != nil {
		return nil, err
	}

	return db, nil
}

func getDaily() (p *Pastes, err error) {
	url := "https://psbdmp.cc/api/v3/getbydate"
	method := "POST"

	year, month, day := time.Now().Date()
	searchDate := fmt.Sprintf("from=%d.%d.%d&to=%d.%d.%d", day, int(month), year, day, int(month), year)

	payload := strings.NewReader(searchDate)

	client := &http.Client{}
	req, err := http.NewRequest(method, url, payload)

	if err != nil {
		fmt.Println(err)
		return &Pastes{}, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("cache-control", "no-cache")

	// Sometimes there are Cloudflare Gateway errors.
	// Check for this condition and retry.
	// I know this is ugly but it was late and I was too
	// lazy to learn a retry package.
	retries := 0
	var res *http.Response
	for retries < 3 {
		res, err = client.Do(req)
		if err != nil {
			log.Println("Encountered error sending HTTP request:", err)
			retries++
		} else if res.StatusCode == 502 {
			log.Printf("Encountered 502 error. Retry [%d/3]", retries+1)
			retries++
		} else {
			break
		}
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	// Using a temporary slice of strings to grab the initial response array
	var tempSlice [][]ID

	err = json.Unmarshal(body, &tempSlice)
	if err != nil {
		return &Pastes{}, err
	}

	// Create a `Pastes` struct and iterate `tempSlice` while appending
	// to `Pastes`
	p = &Pastes{}
	for i := range tempSlice[0] {
		p.Data = append(p.Data, tempSlice[0][i])
	}

	return p, nil

}

func addID(db *sql.DB, pasteID string) error {
	stmt, err := db.Prepare("INSERT INTO pastes(paste_ID) VALUES(?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	_, err = tx.Stmt(stmt).Exec(pasteID)
	if err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

func checkID(db *sql.DB, id string) (ok bool, err error) {
	stmt, err := db.Prepare("SELECT id FROM pastes WHERE paste_ID=?")
	if err != nil {
		return false, err
	}
	defer stmt.Close()

	row := stmt.QueryRow(id)

	var rowid int

	err = row.Scan(&rowid)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}

	return true, nil
}

func post(payload []byte, url string) error {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "psbmon")

	client := &http.Client{}
	_, err = client.Do(req)
	if err != nil {
		return err
	}

	return nil
}

func (id *ID) send(host string) error {
	body, err := json.Marshal(id.ID)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/event", host)
	err = post(body, url)
	if err != nil {
		return err
	}

	return nil
}

func init() {
	_, err := parser.Parse()
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}
}

func main() {
	db, err := open()
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
		return
	}

	err = makeTables(db)
	if err != nil {
		log.Fatalf("failed to create `pastes` table: %v", err)
	}

	hostString = os.Getenv("DG_HOST")

	if hostString == "" {
		log.Fatal("Must provide Dr.Gero API host in DG_HOST environment variable")
		return
	}

	ticker := time.NewTicker(time.Duration(opts.Interval) * time.Minute).C

	log.Println("Starting psbdmp.cc Monitor")
	for {
		select {
		case <-ticker:
			log.Println("Checking psbdmp.cc for updates")

			p, err := getDaily()
			if err != nil {
				log.Printf("error checking daily pastes: %v", err)
				break
			}

			for _, id := range p.Data {
				ok, err := checkID(db, id.ID)
				if err != nil || !ok {
					log.Printf("failed to query DB for paste_ID: %s : %v", id.ID, err)
					break
				}
				err = addID(db, id.ID)
				if err != nil && err.Error() != "UNIQUE constraint failed: pastes.paste_ID" {
					log.Printf("error saving paste_ID to DB: %v", err)
					continue
				} else if err != nil && err.Error() == "UNIQUE constraint failed: pastes.paste_ID" {
					continue
				}
				err = id.send(hostString)
				if err != nil {
					log.Println(err)
				}
			}
		}
	}
}
