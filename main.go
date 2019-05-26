package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	log "github.com/sirupsen/logrus"
)

// Contact defines as contact as saved in the postgres table for sgs.com
type contact struct {
	ID           string    `db:"id"`
	Name         string    `db:"name"`
	Email        string    `db:"email"`
	Phone        string    `db:"phone"`
	Message      string    `db:"message"`
	CaptchaScore string    `db:"captcha_score"`
	Acknowledged bool      `db:"acknowledged"`
	CreatedOn    time.Time `db:"created_on"`
	UpdatedOn    time.Time `db:"updated_on"`
}

var isDev bool

func (c contact) String() string {
	return fmt.Sprintf("Contact name: %s, email: %s, phone: %s", c.Name, c.Email, c.Phone)
}

func init() {
	if _, exists := os.LookupEnv("DEV"); exists {
		// this is the dev environment, write to console and set var
		isDev = true
		log.SetOutput(os.Stdout)
		log.SetLevel(log.DebugLevel)
	} else {
		// this is prod, write to a file
		// this block will failed if ran in prod without sudo priviliges
		if f, err := os.Create("/var/log/sgs/notifier.log"); err != nil {
			log.SetOutput(os.Stdout)
		} else {
			log.SetOutput(f)
		}
	}
}

func main() {
	var (
		err error
		dbx *sqlx.DB
	)
	// Make sure we can connect
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbx, err = sqlx.ConnectContext(ctx, "postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Failed to set up postgres conn: %v", err)
	}

	// new contact loop
	tickChan := time.Tick(3 * time.Hour)
	for {
		select {
		case <-tickChan:
			loc, err := time.LoadLocation("America/New_York")
			if err != nil {
				log.Errorf("Failed to load EST location for date data, please contact webmaster")
			}
			currTime := time.Now()
			start := time.Date(currTime.Year(), currTime.Month(), currTime.Day(), 9, 00, 00, 00, loc)
			end := time.Date(currTime.Year(), currTime.Month(), currTime.Day(), 15, 00, 00, 00, loc)
			if currTime.Before(start) ||
				currTime.After(end) ||
				currTime.Weekday() == time.Saturday ||
				currTime.Weekday() == time.Sunday {
				// if outside work hours, don't message
				log.Info("Outside of work hours, skipping notifications")
				continue
			}
			log.Infof("Checking sgs.com contacts table at: %v", time.Now().String())
			if err := checkContacts(dbx); err != nil {
				// there was an error checking for new contacts, log and report
				log.Errorf("Failed to check postgres for new contacts on sgs.com: %v", err)
			}
		}
	}
}

func checkContacts(dbx *sqlx.DB) error {
	var res []contact
	var q = `SELECT
				id, name, email, phone, message, captcha_score, acknowledged, created_on, updated_on
			FROM
				contacts
			WHERE
				acknowledged = false`
	if err := dbx.Select(&res, q); err != nil {
		log.Debug(err)
		return err
	}
	twilioSID := os.Getenv("TWILIO_ACCOUNT_SID")
	twilioAuth := os.Getenv("TWILIO_AUTH_TOKEN")
	if twilioSID == "" || twilioAuth == "" {
		return fmt.Errorf("Invalid twilio credentials, please check those on the server env and try again")
	}
	for _, r := range res {
		log.Infof("Contact %s is unacknowledged, notifying...", r.Name)
		if err := sendToPOC(r, twilioSID, twilioAuth); err != nil {
			// An error occurred sending contact info to sgs admins. Log it
			log.Errorf("Failed to send contact %v to POC: %v", r.String(), err)
		}
		time.Sleep(15 * time.Second) // Give it some time before sending next contact
	}
	log.Infof("Done sending contacts to sgs owner, returning to idle loop")
	return nil
}

func sendToPOC(c contact, sid, auth string) error {
	var (
		urlStr = "https://api.twilio.com/2010-04-01/Accounts/" + sid + "/Messages.json"
		client = &http.Client{}
		err    error
	)
	// Format the message to send to sgs admins
	msg := formatMessage(c)

	// Set up the request
	req, _ := http.NewRequest("POST", urlStr, &msg)
	req.SetBasicAuth(sid, auth)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	// Send it!
	resp, _ := client.Do(req)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var data map[string]interface{}
		decoder := json.NewDecoder(resp.Body)
		if e := decoder.Decode(&data); e != nil {
			log.Debugf("Failed to parse response after sending message: %v", e)
		}
		log.Debugf("Response from sent message: %v", data)
	} else {
		err = fmt.Errorf("Failed to send message to contact. Issue: %v", resp.Status)
	}
	return err
}

func formatMessage(c contact) strings.Reader {
	var msgToPOC = "We are being contacted by '%s' with email: '%s' and phone number '%s'" +
		"for the following reason: '%s'.\n" +
		"Please acknowledged receipt of this contact by replying '%s' to this message."
	msgData := url.Values{}
	msgData.Set("From", os.Getenv("TWILIO_FROM_NUMBER"))
	msgData.Set("To", os.Getenv("TWILIO_TO_NUMBER"))
	msgData.Set("provideFeedback", "true")
	msgData.Set("Body", fmt.Sprintf(msgToPOC, c.Name, c.Email, c.Phone, c.Message, c.ID))
	return *strings.NewReader(msgData.Encode())
}
