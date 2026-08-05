// Command pushtest sends one Expo push notification and prints what Expo
// answered.
//
// The in-app diagnostics screen covers the normal case, but when the question
// is "is this token dead or is our server not sending?", this takes the server
// out of the loop entirely:
//
//	go run ./cmd/pushtest "ExponentPushToken[xxxxxxxx]"
//	go run ./cmd/pushtest "ExponentPushToken[xxxxxxxx]" "Sarlavha" "Matn"
//
// A "DeviceNotRegistered" ticket means the app was reinstalled and the stored
// token is stale; an "ok" ticket means Expo accepted it and the phone should
// buzz within a second or two.
package main

import (
	"fmt"
	"os"

	"prostaff/internal/push"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pushtest <ExponentPushToken[...]> [title] [body]")
		os.Exit(2)
	}

	token := os.Args[1]
	title := "Test bildirishnoma"
	body := "Push ishlayapti — bu xabar pushtest orqali yuborildi."
	if len(os.Args) > 2 {
		title = os.Args[2]
	}
	if len(os.Args) > 3 {
		body = os.Args[3]
	}

	tickets, err := push.SendSync(push.Urgent(token, title, body,
		map[string]any{"type": "test.push"}, 60))
	if err != nil {
		fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
		os.Exit(1)
	}

	failed := false
	for i, ticket := range tickets {
		fmt.Printf("[%d] status=%s id=%s %s\n", i, ticket.Status, ticket.ID, ticket.Message)
		if ticket.Status != "ok" {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println("accepted by Expo — the device should show it now")
}
