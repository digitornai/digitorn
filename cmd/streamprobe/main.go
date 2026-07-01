// Command streamprobe replays the background service's reply:stream flow against a
// LIVE daemon (Launch StreamReply + StreamReplies) and prints every item the stream
// would relay to a channel. It proves the agentic-loop streaming end-to-end without
// needing Telegram/Discord — a deterministic stand-in for the live test.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/digitornai/digitorn/internal/background/daemonclient"
)

func main() {
	jwt := readJWT()
	c := daemonclient.New("http://127.0.0.1:8000", jwt, daemonclient.WithPollInterval(300*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	app := "dcapprove"
	if len(os.Args) > 1 {
		app = os.Args[1]
	}
	msg := "Bonjour ! Réponds en une courte phrase."
	if len(os.Args) > 2 {
		msg = os.Args[2]
	}
	// Mode: "stream" (default, reply:stream → StreamReplies) or "auto"
	// (reply:auto → WaitForReply, returns only the FINAL answer of the turn).
	mode := "stream"
	if len(os.Args) > 3 {
		mode = os.Args[3]
	}
	sid := fmt.Sprintf("streamprobe-%d", time.Now().UnixNano())

	if mode == "auto" {
		res, err := c.Launch(ctx, daemonclient.LaunchSpec{
			AppID:        app,
			SessionID:    sid,
			Message:      msg,
			Model:        "moonshotai/kimi-k2.6",
			WaitForReply: true,
			ReplyTimeout: 110 * time.Second,
		}, "streamprobe-job")
		if err != nil {
			fmt.Println("LAUNCH ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("launched session=%s user_seq=%d created=%v\n", res.SessionID, res.UserSeq, res.Created)
		fmt.Printf("  FINAL reply seq=%d text=%q\n", res.ReplySeq, res.Reply)
		if res.Reply == "" {
			os.Exit(2)
		}
		return
	}

	res, err := c.Launch(ctx, daemonclient.LaunchSpec{
		AppID:       app,
		SessionID:   sid,
		Message:     msg,
		Model:       "moonshotai/kimi-k2.6",
		StreamReply: true,
	}, "streamprobe-job")
	if err != nil {
		fmt.Println("LAUNCH ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("launched session=%s user_seq=%d created=%v\n", res.SessionID, res.UserSeq, res.Created)

	n := 0
	serr := c.StreamReplies(ctx, app, res.SessionID, res.UserSeq, func(item daemonclient.StreamItem) {
		n++
		fmt.Printf("  ITEM #%d kind=%s seq=%d text=%q\n", n, item.Kind, item.Seq, item.Text)
	})
	fmt.Printf("stream done: items=%d err=%v\n", n, serr)
	if n == 0 {
		os.Exit(2)
	}
}

func readJWT() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".digitorn", "credentials.json"))
	if err != nil {
		fmt.Println("read creds:", err)
		os.Exit(1)
	}
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(b, &cred)
	return cred.AccessToken
}
