package reviewer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNativeHistoryTransactionRejectsDowngradeAndLateEvents(t *testing.T) {
	for _, mutation := range []string{"nil collection", "missing event", "changed actor", "late request"} {
		t.Run(mutation, func(t *testing.T) {
			s, _ := openFixtureStore(t)
			batch := Discovery{Scope: alphaScope(), Requests: []NativeRequest{native(1, controlTime.Add(time.Minute))}, NativeEvents: []NativeHistoryEvent{{ID: 1, ActorID: OwnerID, RequesterID: OwnerID, BotID: 9003, Kind: "review_requested", CreatedAt: controlTime.Add(time.Minute)}}, Through: controlTime.Add(2 * time.Minute), Complete: true}
			if err := s.Discover(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Snapshot(context.Background())
			batch.Through = controlTime.Add(3 * time.Minute)
			switch mutation {
			case "nil collection":
				batch.NativeEvents = nil
			case "missing event":
				batch.NativeEvents = []NativeHistoryEvent{}
			case "changed actor":
				batch.NativeEvents[0].ActorID = 700
				batch.NativeEvents[0].RequesterID = 700
			case "late request":
				batch.NativeEvents = append(batch.NativeEvents, NativeHistoryEvent{ID: 2, ActorID: OwnerID, RequesterID: OwnerID, BotID: 9003, Kind: "review_requested", CreatedAt: controlTime.Add(2 * time.Minute)})
				batch.Requests = append(batch.Requests, native(2, controlTime.Add(2*time.Minute)))
			}
			if err := s.Discover(context.Background(), batch); !errors.Is(err, ErrDeferred) {
				t.Fatal("untrusted native history advanced transaction", err)
			}
			after, _ := s.Snapshot(context.Background())
			if Digest(before) != Digest(after) {
				t.Fatal("failed native history changed state")
			}
		})
	}
}
