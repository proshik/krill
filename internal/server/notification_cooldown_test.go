package server

import (
	"testing"
	"time"
)

// A double click on "Send test message" sends one message, not two.
func TestClaimTestNotificationCooldown(t *testing.T) {
	s := &Server{}
	now := time.Now()
	if !s.claimTestNotification(1, now) {
		t.Fatal("the first test message must be allowed")
	}
	if s.claimTestNotification(1, now.Add(time.Second)) {
		t.Fatal("a second one within the cooldown must be refused")
	}
	if !s.claimTestNotification(2, now.Add(time.Second)) {
		t.Fatal("the cooldown is per organization")
	}
	if !s.claimTestNotification(1, now.Add(testNotificationCooldown)) {
		t.Fatal("after the cooldown a test message is allowed again")
	}
}
