package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pragmader/q3-server-bot/pkg/events"
	"github.com/sirupsen/logrus"
)

var (
	ErrBotRunning = errors.New("this bot is already running")
)

// TelegramBot represents a bot built for Telegram
type TelegramBot interface {
	// Starts the bot process
	Start(context.Context) error
	// Close stops the process and cleans up
	Close() error
}

type ParseMode string

var (
	Mardown ParseMode = "Markdown"
	HTML    ParseMode = "HTML"
)

// Taken from https://core.telegram.org/bots/api#sendmessage
type telegramMessage struct {
	// ChatID — Unique identifier for the target chat or username of
	// the target channel (in the format @channelusername)
	ChatID string `json:"chat_id"`
	// Text — Text of the message to be sent, 1-4096 characters after entities parsing
	Text string `json:"text"`
	// ParseMode — Send Markdown or HTML, if you want Telegram apps to show bold,
	// italic, fixed-width text or inline URLs in your bot's message.
	ParseMode *ParseMode `json:"parse_mode,omitempty"`
	// DisableNotification — Sends the message silently.
	// Users will receive a notification with no sound.
	DisableNotification bool `json:"disable_notification,omitempty"`
}

// NewServerEventsBot creates a new bot that sends server events to the given receiver.
// `token` — token of the bot on Telegram
// `chatID` — Unique identifier for the target chat or username of the target channel
// (in the format @channelusername).
// `throttling` — duration of time in which all incoming events would be grouped
// into one message.
func NewServerEventsBot(mngr events.Manager, token, chatID string, throttling time.Duration) TelegramBot {
	return serverEventsBot{
		mngr:       mngr,
		token:      token,
		chatID:     chatID,
		events:     mngr.Subscribe(),
		throttling: throttling,
	}
}

type serverEventsBot struct {
	mngr       events.Manager
	token      string
	chatID     string
	events     <-chan events.AnyEvent
	throttling time.Duration
	runningCnt int32
	closed     chan bool
}

func (b serverEventsBot) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&b.runningCnt, 0, 1) {
		return ErrBotRunning
	}

	defer func() {
		atomic.SwapInt32(&b.runningCnt, 0)
	}()

	logrus := logrus.
		WithField("package", "telegram").
		WithField("module", "ServerEventsBot").
		WithField("function", "Start")

	var (
		mutex        sync.Mutex
		currentBatch []events.AnyEvent
	)

	go func() {
		logrus.Debug("starting to enumerate events...")
		for e := range b.events {
			logrus := logrus.WithField("event.type", fmt.Sprintf("%T", e))
			logrus.Debug("received event")
			mutex.Lock()
			currentBatch = append(currentBatch, e)
			mutex.Unlock()
		}
	}()

	logrus.Debug("starting message sending loop...")
	for {
		logrus.Debugf("sending messages again in %s...", b.throttling)
		select {
		case <-ctx.Done():
			logrus.Debug("got context cancellation, stopped")
			return ctx.Err()
		case <-b.closed:
			logrus.Debug("got closing signal, stopped")
			return nil
		case <-time.After(b.throttling):
			mutex.Lock()
			logrus.
				WithField("event.count", len(currentBatch)).
				Debug("starting iteration...")
			if len(currentBatch) != 0 {
				sendErr := b.sendMessage(ctx, currentBatch)
				if sendErr != nil {
					logrus.Warnf("failed to send message: %s", sendErr.Error())
				} else {
					currentBatch = nil
				}
			}
			mutex.Unlock()
		}
	}

	return nil
}

func (b serverEventsBot) sendMessage(ctx context.Context, eventList []events.AnyEvent) (err error) {
	logrus := logrus.
		WithField("package", "telegram").
		WithField("module", "ServerEventsBot").
		WithField("function", "sendMessage").
		WithField("event.count", len(eventList))

	msg := b.buildMessage(eventList)
	if err != nil {
		return err
	}

	r, w := io.Pipe()

	go func() {
		enc := json.NewEncoder(w)
		err = enc.Encode(msg)
		if err != nil {
			w.CloseWithError(err)
		} else {
			w.Close()
		}
	}()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	logrus.Debug("making request to Telegram API")
	resp, err := http.Post(url, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	logrus.
		WithField("status", resp.StatusCode).
		Debug("go response from Telegram API")

	if resp.StatusCode != http.StatusOK {
		lr := io.LimitReader(resp.Body, 1024) // read max 1KB of response
		respBody, err := ioutil.ReadAll(lr)
		if err != nil {
			return err
		}
		return fmt.Errorf(
			"unexpected status code %d, expected %d. Body: %s",
			resp.StatusCode,
			http.StatusOK,
			string(respBody),
		)
	}

	return nil
}

func (b serverEventsBot) buildMessage(eventList []events.AnyEvent) (msg telegramMessage) {
	logrus := logrus.
		WithField("package", "telegram").
		WithField("module", "ServerEventsBot").
		WithField("function", "buildMessage").
		WithField("event.count", len(eventList))

	logrus.Debug("building the message...")
	defer logrus.Debug("message has been built")

	msg = telegramMessage{
		ChatID:    b.chatID,
		ParseMode: &Mardown,
	}

	var (
		joinedPlayers, leftPlayers []string
		serverMsg                  string
	)

	for _, e := range eventList {
		switch te := e.(type) {
		case *events.ServerStartedEvent:
			serverMsg = "Server started. Join the game!"
		case *events.ServerStoppedEvent:
			serverMsg = "Server stopped."
		case *events.PlayerJoinedEvent:
			joinedPlayers = append(joinedPlayers, "`"+te.Player.GetName()+"`") // bold formatting
		case *events.PlayerLeftEvent:
			leftPlayers = append(leftPlayers, "`"+te.Player.GetName()+"`")
		}
	}

	if serverMsg != "" {
		msg.Text = serverMsg + "\n"
	}

	if len(joinedPlayers) != 0 {
		msg.Text += fmt.Sprintf("\n%s joined the game", strings.Join(joinedPlayers, ", "))
	}
	if len(leftPlayers) != 0 {
		msg.Text += fmt.Sprintf("\n%s left the game", strings.Join(leftPlayers, ", "))
	}

	return msg
}

func (b serverEventsBot) Close() error {
	logrus := logrus.
		WithField("package", "telegram").
		WithField("module", "ServerEventsBot").
		WithField("function", "sendMessage")

	logrus.Debug("closing...")
	b.closed <- true
	return b.mngr.Unsubscribe(b.events)
}
