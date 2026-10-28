package alert

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
)

func testTarget(id int64, name string) monitor.Target {
	return monitor.Target{
		ID:              id,
		Name:            name,
		URL:             "https://example.com/healthz",
		IntervalSeconds: 30,
		TimeoutSeconds:  10,
		Enabled:         true,
	}
}

func upResult(targetID int64) monitor.Result {
	return monitor.Result{
		TargetID:   targetID,
		Status:     monitor.StatusUp,
		StatusCode: 200,
		LatencyMS:  12,
		CheckedAt:  time.Now().UTC(),
	}
}

func downResult(targetID int64, statusCode int) monitor.Result {
	return monitor.Result{
		TargetID:   targetID,
		Status:     monitor.StatusDown,
		StatusCode: statusCode,
		LatencyMS:  500,
		Error:      "unexpected status 500 Internal Server Error",
		CheckedAt:  time.Now().UTC(),
	}
}

func TestStateMachineNotifiesOnceAfterThreshold(t *testing.T) {
	machine := NewStateMachine(3)
	target := testTarget(1, "API")

	for i := 1; i <= 2; i++ {
		if _, ok := machine.Observe(target, downResult(target.ID, 500)); ok {
			t.Fatalf("notification on failure %d, want silence before threshold", i)
		}
	}

	notification, ok := machine.Observe(target, downResult(target.ID, 500))
	if !ok {
		t.Fatal("no notification on third failure, want DOWN")
	}
	if notification.Event != EventDown {
		t.Errorf("Event = %q, want down", notification.Event)
	}
	if notification.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", notification.ConsecutiveFailures)
	}
	if notification.PreviousStatus != monitor.StatusUnknown {
		t.Errorf("PreviousStatus = %q, want unknown", notification.PreviousStatus)
	}
	if machine.Status(target.ID) != monitor.StatusDown {
		t.Error("machine status is not down after notification")
	}

	// Дальше цель продолжает падать — это уже не новость.
	for i := 0; i < 5; i++ {
		if _, ok := machine.Observe(target, downResult(target.ID, 500)); ok {
			t.Fatal("repeated notification while already down")
		}
	}

	notification, ok = machine.Observe(target, upResult(target.ID))
	if !ok {
		t.Fatal("no notification on recovery, want UP")
	}
	if notification.Event != EventUp {
		t.Errorf("Event = %q, want up", notification.Event)
	}
	if notification.PreviousStatus != monitor.StatusDown {
		t.Errorf("PreviousStatus = %q, want down", notification.PreviousStatus)
	}

	if _, ok := machine.Observe(target, upResult(target.ID)); ok {
		t.Fatal("repeated notification while already up")
	}
}

func TestStateMachineSingleFailureWithThresholdOne(t *testing.T) {
	machine := NewStateMachine(1)
	target := testTarget(1, "API")

	notification, ok := machine.Observe(target, downResult(target.ID, 502))
	if !ok || notification.Event != EventDown {
		t.Fatalf("notification = %+v, ok = %v; want down", notification, ok)
	}
}

func TestStateMachineFailureCounterResetsOnRecovery(t *testing.T) {
	machine := NewStateMachine(3)
	target := testTarget(1, "API")

	// Два падения, затем успех: счётчик должен обнулиться.
	machine.Observe(target, downResult(target.ID, 500))
	machine.Observe(target, downResult(target.ID, 500))
	machine.Observe(target, upResult(target.ID))

	machine.Observe(target, downResult(target.ID, 500))
	machine.Observe(target, downResult(target.ID, 500))
	if _, ok := machine.Observe(target, downResult(target.ID, 500)); !ok {
		t.Fatal("threshold was not reached after the counter reset")
	}
}

func TestStateMachineSeed(t *testing.T) {
	t.Run("seed down does not repeat alert", func(t *testing.T) {
		machine := NewStateMachine(3)
		target := testTarget(1, "API")
		machine.Seed(target.ID, monitor.StatusDown)

		for i := 0; i < 3; i++ {
			if _, ok := machine.Observe(target, downResult(target.ID, 500)); ok {
				t.Fatal("notification after restart while target is still down")
			}
		}

		if notification, ok := machine.Observe(target, upResult(target.ID)); !ok || notification.Event != EventUp {
			t.Fatalf("notification = %+v, ok = %v; want recovery", notification, ok)
		}
	})

	t.Run("seed up still alerts on failures", func(t *testing.T) {
		machine := NewStateMachine(2)
		target := testTarget(1, "API")
		machine.Seed(target.ID, monitor.StatusUp)

		machine.Observe(target, downResult(target.ID, 500))
		if _, ok := machine.Observe(target, downResult(target.ID, 500)); !ok {
			t.Fatal("no DOWN notification after seeded up state")
		}
	})
}

func TestStateMachineIsolatesTargets(t *testing.T) {
	machine := NewStateMachine(2)
	first := testTarget(1, "API")
	second := testTarget(2, "Web")

	machine.Observe(first, downResult(first.ID, 500))
	if _, ok := machine.Observe(second, downResult(second.ID, 500)); ok {
		t.Fatal("failure of one target must not trigger alert for another")
	}
	if machine.Status(first.ID) != monitor.StatusUnknown {
		t.Error("first target reached threshold too early")
	}
}

func TestStateMachineForget(t *testing.T) {
	machine := NewStateMachine(1)
	target := testTarget(1, "API")

	machine.Observe(target, downResult(target.ID, 500))
	machine.Forget(target.ID)

	if machine.Status(target.ID) != monitor.StatusUnknown {
		t.Error("status after Forget is not unknown")
	}

	if _, ok := machine.Observe(target, downResult(target.ID, 500)); !ok {
		t.Error("target after Forget must alert again")
	}
}

func TestStateMachineZeroThresholdFallsBackToOne(t *testing.T) {
	machine := NewStateMachine(0)
	target := testTarget(1, "API")

	if _, ok := machine.Observe(target, downResult(target.ID, 500)); !ok {
		t.Error("threshold 0 must behave as 1")
	}
}

type recordingNotifier struct {
	mu            sync.Mutex
	notifications []Notification
	err           error
}

func (n *recordingNotifier) Notify(_ context.Context, notification Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.notifications = append(n.notifications, notification)
	return n.err
}

func TestMultiNotifier(t *testing.T) {
	first := &recordingNotifier{}
	second := &recordingNotifier{err: errors.New("telegram is down")}

	multi := MultiNotifier{first, nil, second}
	err := multi.Notify(context.Background(), Notification{
		Event:  EventDown,
		Target: testTarget(1, "API"),
		Result: downResult(1, 500),
	})

	if err == nil {
		t.Fatal("Notify returned nil error, want joined error")
	}
	if len(first.notifications) != 1 {
		t.Errorf("first notifier calls = %d, want 1", len(first.notifications))
	}
	if len(second.notifications) != 1 {
		t.Errorf("second notifier calls = %d, want 1: one broken channel must not block others", len(second.notifications))
	}
}

func TestLogNotifier(t *testing.T) {
	var buf bytes.Buffer
	notifier := &LogNotifier{Logger: log.New(&buf, "", 0)}

	err := notifier.Notify(context.Background(), Notification{
		Event:               EventDown,
		Target:              testTarget(7, "API"),
		Result:              downResult(7, 500),
		ConsecutiveFailures: 3,
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	output := buf.String()
	for _, want := range []string{"alert:", "DOWN: API", "https://example.com/healthz", "Failed checks: 3"} {
		if !strings.Contains(output, want) {
			t.Errorf("log output does not contain %q:\n%s", want, output)
		}
	}
}

func TestFormat(t *testing.T) {
	down := Format(Notification{
		Event:               EventDown,
		Target:              testTarget(1, "API"),
		Result:              downResult(1, 503),
		ConsecutiveFailures: 4,
	})
	for _, want := range []string{"DOWN: API", "URL: https://example.com/healthz", "HTTP: 503", "Failed checks: 4"} {
		if !strings.Contains(down, want) {
			t.Errorf("down message does not contain %q:\n%s", want, down)
		}
	}

	up := Format(Notification{
		Event:  EventUp,
		Target: testTarget(1, "API"),
		Result: upResult(1),
	})
	if !strings.Contains(up, "RECOVERED: API") {
		t.Errorf("recovery message = %q, want RECOVERED", up)
	}
	if strings.Contains(up, "Failed checks") {
		t.Errorf("recovery message must not mention failed checks:\n%s", up)
	}
}
