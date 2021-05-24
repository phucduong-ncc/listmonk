package mailbox

import (
	"crypto/tls"
	"fmt"
	"log"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

// IMAP represents a IMAP
type IMAP struct {
	opt    Opt
	client *client.Client
}

// NewIMAP returns a new instance of the POP mailbox client.
func NewIMAP(opt Opt) *IMAP {
	return &IMAP{
		opt: opt,
	}
}

func (i *IMAP) newClient() error {
	var (
		c   *client.Client
		err error

		addr = fmt.Sprintf("%s:%d", i.opt.Host, i.opt.Port)
	)

	if i.opt.TLSEnabled {
		// TLS connection.
		tlsCfg := &tls.Config{}
		if i.opt.TLSSkipVerify {
			tlsCfg.InsecureSkipVerify = i.opt.TLSSkipVerify
		} else {
			tlsCfg.ServerName = i.opt.Host
		}

		c, err = client.DialTLS(addr, tlsCfg)
	} else {
		// Non-TLS connection.
		c, err = client.Dial(addr)
	}

	if err != nil {
		return err
	}
	i.client = c

	// Authenticate.
	if i.opt.Username != "" {
		if err := i.client.Login(i.opt.Username, i.opt.Password); err != nil {
			return err
		}
	}

	// Select the mailbox to scan.
	mbox, err := c.Select(i.opt.Folder, false)
	if err != nil {
		return err
	}

	// Get the last 4 messages
	fmt.Println("total=", mbox.Messages)
	seqset := new(imap.SeqSet)
	seqset.AddRange(mbox.Messages, mbox.Messages)

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
	}()

	log.Println("Last 4 messages:")
	for msg := range messages {
		log.Println("* "+msg.Envelope.Subject, msg.Envelope.Date)

	}

	var section imap.BodySectionName
	section.Specifier = imap.HeaderSpecifier

	items := []imap.FetchItem{section.FetchItem()}

	messages = make(chan *imap.Message, 1)
	if err := c.Fetch(seqset, items, messages); err != nil {
		return err
	}

	msg := <-messages
	if msg == nil {
		log.Fatal("Server didn't returned message")
	}

	r := msg.GetBody(&section)
	if r == nil {
		log.Fatal("Server didn't returned message body")
	}

	// Create a new mail reader
	mr, err := mail.CreateReader(r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(mr)

	// fmt.Println("ID = ", mr.Header.Get(idHeader))
	return nil
}
