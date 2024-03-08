package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sethlaw/EasyEASM/pkg/active"
	"github.com/sethlaw/EasyEASM/pkg/configparser"
	"github.com/sethlaw/EasyEASM/pkg/passive"
	"github.com/sethlaw/EasyEASM/pkg/utils"
)

func main() {
	// install required tools
	utils.InstallTools()

	// print a banner
	banner := "\x1b[36m****************\n\nEASY EASM\n\n***************\x1b[0m\n"
	fmt.Println(banner)

	// parse the configuration file
	cfg := configparser.ParseConfig()

	// db setup
	if _, err := os.Stat("easyeasm.db"); err == nil {
	} else {
		setupDB()
		// db, _ := sql.Open("sqlite3", "./easyeasm.db")
	}
	db, _ := sql.Open("sqlite3", "./easyeasm.db")

	// check for previous run file
	if _, err := os.Stat("EasyEASM.csv"); err == nil {
		fmt.Println("Found data from previous run!")
		e := os.Rename("EasyEASM.csv", "old_EasyEASM.csv")
		if e != nil {
			panic(e)
		}
		var domains = getActiveDomains(db)
		fmt.Println("Active domains from previous run: ", len(domains))
	} else {
		fmt.Println("No previous run data found")
	}
	var newActiveDomains = []string{}
	var newLiveDomains = []string{}
	var deprecatedActiveDomains = []string{}
	var deprecatedLiveDomains = []string{}

	// create a PassiveRunner instance
	Runner := passive.PassiveRunner{
		SeedDomains: cfg.RunConfig.Domains,
	}

	// run passive enumeration and get the results
	passiveResults := Runner.RunPassiveEnum()

	// remove duplicate subdomains
	Runner.Subdomains = utils.RemoveDuplicates(passiveResults)
	Runner.Results = len(Runner.Subdomains)

	fmt.Printf("\x1b[31mFound %d subdomains\n\n\x1b[0m", Runner.Results)
	fmt.Println(Runner.Subdomains)
	fmt.Println("Checking which domains are live and generating assets csv...")
	for _, domain := range Runner.Subdomains {
		if !domainExists(db, domain) {
			insertDomain(db, domain)
			newActiveDomains = append(newActiveDomains, domain)
		} else if !domainIsActive(db, domain) {
			updateActiveDomain(db, domain, true)
			newActiveDomains = append(newActiveDomains, domain)
		}
	}

	oldActiveDomains := getActiveDomains(db)
	for _, domain := range oldActiveDomains {
		if !contains(newActiveDomains, domain) {
			deprecatedActiveDomains = append(deprecatedActiveDomains, domain)
			updateActiveDomain(db, domain, false)
		}
	}

	// check the run type specified in the config and perform actions accordingly
	if strings.ToLower(cfg.RunConfig.RunType) == "fast" {
		// fast run: passive enumeration only

		// run Httpx to check live domains
		Runner.RunHttpx()
		for _, domain := range Runner.Subdomains {
			if !domainExists(db, domain) {
				insertDomain(db, domain)
				newLiveDomains = append(newLiveDomains, domain)
			}
			if !domainIsLive(db, domain) {
				updateLiveDomain(db, domain, true)
				newLiveDomains = append(newLiveDomains, domain)
			}
		}

		oldLiveDomains := getLiveDomains(db)
		for _, domain := range oldLiveDomains {
			if !contains(newLiveDomains, domain) {
				deprecatedLiveDomains = append(deprecatedLiveDomains, domain)
				updateLiveDomain(db, domain, false)
			}
		}

		notifyDomains(newActiveDomains, newLiveDomains, deprecatedActiveDomains, deprecatedLiveDomains, cfg.RunConfig.SlackWebhook)

	} else if strings.ToLower(cfg.RunConfig.RunType) == "complete" {
		// complete run: active enumeration

		// active enumeration
		ActiveRunner := active.ActiveRunner{
			SeedDomains: cfg.RunConfig.Domains,
		}
		activeResults := ActiveRunner.RunActiveEnum(cfg.RunConfig.ActiveWordlist, cfg.RunConfig.ActiveThreads)
		activeResults = append(activeResults, passiveResults...)

		ActiveRunner.Subdomains = utils.RemoveDuplicates(activeResults)

		// permutation scan
		permutationResults := ActiveRunner.RunPermutationScan(cfg.RunConfig.ActiveThreads)
		ActiveRunner.Subdomains = append(ActiveRunner.Subdomains, permutationResults...)
		ActiveRunner.Subdomains = utils.RemoveDuplicates(ActiveRunner.Subdomains)
		ActiveRunner.Results = len(ActiveRunner.Subdomains)

		// httpx scan
		fmt.Printf("Found %d subdomains: ", ActiveRunner.Results)
		fmt.Println(ActiveRunner.Subdomains)
		fmt.Println("Checking which domains are live and generating assets csv...")
		ActiveRunner.RunHttpx()
		for _, domain := range ActiveRunner.Subdomains {
			if !domainExists(db, domain) {
				insertDomain(db, domain)
				newLiveDomains = append(newLiveDomains, domain)
			}
			if !domainIsLive(db, domain) {
				updateLiveDomain(db, domain, true)
				newLiveDomains = append(newLiveDomains, domain)
			}
		}
		oldLiveDomains := getLiveDomains(db)
		for _, domain := range oldLiveDomains {
			if !contains(newLiveDomains, domain) {
				deprecatedLiveDomains = append(deprecatedLiveDomains, domain)
				updateLiveDomain(db, domain, false)
			}
		}

		// notify about new domains
		notifyDomains(newActiveDomains, newLiveDomains, deprecatedActiveDomains, deprecatedLiveDomains, cfg.RunConfig.SlackWebhook)
	} else {
		// invalid run mode specified
		panic("Please pick a valid run mode and add it to your config.yml file! You can set runType to either 'fast' or 'complete'")
	}
	db.Close() // Close the SQLite File
}

func notifyDomains(newActiveDomains []string, newLiveDomains []string, deprecatedActiveDomains []string, deprecatedLiveDomains []string, slackWebhook string) {
	if len(newActiveDomains) > 0 {
		sendToSlack(slackWebhook, fmt.Sprintf("New active subdomain records: %v", newActiveDomains))
	}
	if len(newLiveDomains) > 0 {
		sendToSlack(slackWebhook, fmt.Sprintf("New live subdomain hosts: %v", newLiveDomains))
	}
	if len(deprecatedActiveDomains) > 0 {
		sendToSlack(slackWebhook, fmt.Sprintf("Deprecated subdomain records: %v", deprecatedActiveDomains))
	}
	if len(deprecatedLiveDomains) > 0 {
		sendToSlack(slackWebhook, fmt.Sprintf("Deprecated live subdomain hosts: %v", deprecatedLiveDomains))
	}
}

func setupDB() {
	file, err := os.Create("easyeasm.db")
	if err != nil {
		log.Fatal(err.Error())
	}
	file.Close()

	db, _ := sql.Open("sqlite3", "./easyeasm.db") // Open the created SQLite File
	defer db.Close()                              // Defer Closing the database
	createTable(db)                               // Create Database Tables
}

func createTable(db *sql.DB) {
	createDomainTable := `CREATE TABLE domains (
		"id" integer NOT NULL PRIMARY KEY AUTOINCREMENT,		
		"domain" TEXT,
		"active" BOOLEAN,
		"live" BOOLEAN,
		"first_seen" TEXT,
		"last_seen" TEXT		
	  );`

	statement, err := db.Prepare(createDomainTable)
	if err != nil {
		log.Fatal(err.Error())
	}
	statement.Exec()
}

func getDomains(db *sql.DB) []string {
	rows, err := db.Query("SELECT domain FROM domains")
	if err != nil {
		log.Fatal(err)
	}
	var domains []string
	for rows.Next() {
		var domain string
		rows.Scan(&domain)
		domains = append(domains, domain)
	}
	return domains
}

func domainExists(db *sql.DB, domain string) bool {
	rows, err := db.Query("SELECT domain FROM domains WHERE domain = ?", domain)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		return true
	}
	return false
}

func domainIsLive(db *sql.DB, domain string) bool {
	rows, err := db.Query("SELECT domain FROM domains WHERE domain = ? AND live = true", domain)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		return true
	}
	return false
}

func domainIsActive(db *sql.DB, domain string) bool {
	rows, err := db.Query("SELECT domain FROM domains WHERE domain = ? AND active = true", domain)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		return true
	}
	return false
}

func getActiveDomains(db *sql.DB) []string {
	rows, err := db.Query("SELECT domain FROM domains WHERE active = 1")
	if err != nil {
		log.Fatal(err)
	}
	var domains []string
	defer rows.Close()
	for rows.Next() {
		var domain string
		rows.Scan(&domain)
		domains = append(domains, domain)
	}
	return domains
}

func getLiveDomains(db *sql.DB) []string {
	rows, err := db.Query("SELECT domain FROM domains WHERE live = 1")
	if err != nil {
		log.Fatal(err)
	}
	var domains []string
	defer rows.Close()
	for rows.Next() {
		var domain string
		rows.Scan(&domain)
		domains = append(domains, domain)
	}
	return domains
}

func insertDomain(db *sql.DB, domain string) {
	insertSQL := `INSERT INTO domains(domain, active, live, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)`
	statement, err := db.Prepare(insertSQL)
	if err != nil {
		log.Fatalln(err.Error())
	}
	var now = time.Now()
	_, err = statement.Exec(domain, 1, 0, now, now)
	if err != nil {
		log.Fatalln(err.Error())
	}
}

func updateActiveDomain(db *sql.DB, domain string, active bool) {
	updateSQL := `UPDATE domains SET active = ?, last_seen = ? WHERE domain = ?`
	statement, err := db.Prepare(updateSQL)
	if err != nil {
		log.Fatalln(err.Error())
	}
	var now = time.Now()
	_, err = statement.Exec(active, now, domain)
	if err != nil {
		log.Fatalln(err.Error())
	}
}

func updateLiveDomain(db *sql.DB, domain string, live bool) {
	updateSQL := `UPDATE domains SET live = ?, last_seen = ? WHERE domain = ?`
	statement, err := db.Prepare(updateSQL)
	if err != nil {
		log.Fatalln(err.Error())
	}
	var now = time.Now()
	_, err = statement.Exec(live, now, domain)
	if err != nil {
		log.Fatalln(err.Error())
	}
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func sendToSlack(webhookURL string, message string) {
	// Create JSON payload
	payload := map[string]string{
		"text": message,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		fmt.Println("Error creating JSON:", err)
		return
	}

	// Send HTTP POST request
	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		fmt.Println("Error sending to Slack:", err)
		return
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		fmt.Println("Error response from Slack:", resp.Status)
	}
}
