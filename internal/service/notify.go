package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// Notification is one body-free "a human may be needed" observation. It names
// a Room and a fixed kind; it never carries message text, tool input, approval
// detail, session identity, credentials or vendor output, so it is safe to
// hand to an operator-configured command or an OS notification.
type Notification struct {
	Seq      uint64         `json:"seq"`
	RoomID   string         `json:"room_id"`
	RoomName string         `json:"room_name"`
	HostMode model.HostMode `json:"host_mode"`
	Kind     string         `json:"kind"`
	Slot     model.ActorID  `json:"slot,omitempty"`
	At       time.Time      `json:"at"`
}

// Notification kinds. Room engines emit the first group from their own facts;
// the Service derives input_waiting from queue age.
var notificationKinds = map[string]bool{
	"human_turn":         true,
	"approval_requested": true,
	"agent_failed":       true,
	"stall_warning":      true,
	"delivery_uncertain": true,
	"wake_failed":        true,
	"input_waiting":      true,
}

const (
	notificationHistory  = 200
	notificationCooldown = 30 * time.Second
	notifyCommandTimeout = 10 * time.Second
	// Below the default 15-minute idle timeout, so a resumed Room whose wake did
	// not produce a turn still reports its stuck input before it is suspended.
	inputWaitingThreshold = 10 * time.Minute
)

// NotifierConfig selects optional delivery beyond the in-Service list. Command
// is an argv (never a shell string) run once per notification with the JSON
// notification on stdin. It is operator configuration, never Room input.
type NotifierConfig struct {
	Command []string
	Now     func() time.Time
	Run     func(ctx context.Context, argv []string, stdin []byte) error
}

// Notifier keeps a bounded, deduplicated, rate-limited list of notifications
// and fans them out. It is best effort: a failed command or a slow consumer
// never blocks a Room engine or changes Room facts.
type Notifier struct {
	cfg     NotifierConfig
	mu      sync.Mutex
	seq     uint64
	items   []Notification
	seen    map[string]bool
	last    map[string]time.Time
	changed chan struct{}
	queue   chan Notification
	done    chan struct{}
	closed  bool
}

func NewNotifier(cfg NotifierConfig) *Notifier {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Run == nil {
		cfg.Run = runNotifyCommand
	}
	n := &Notifier{cfg: cfg, seen: map[string]bool{}, last: map[string]time.Time{}, changed: make(chan struct{}), queue: make(chan Notification, 64), done: make(chan struct{})}
	go n.deliver()
	return n
}

// Notify records one observation. key identifies the underlying fact so replays
// or repeated events cannot notify twice; a per-Room-and-kind cooldown folds
// bursts into one alert. It reports whether the notification was recorded.
func (n *Notifier) Notify(room Room, kind string, slot model.ActorID, key string) bool {
	if n == nil || !notificationKinds[kind] || room.ID == "" {
		return false
	}
	now := n.cfg.Now()
	n.mu.Lock()
	dedupe := room.ID + "/" + kind + "/" + key
	burst := room.ID + "/" + kind
	if n.closed || n.seen[dedupe] || now.Sub(n.last[burst]) < notificationCooldown {
		n.seen[dedupe] = true
		n.mu.Unlock()
		return false
	}
	n.seen[dedupe] = true
	n.last[burst] = now
	n.seq++
	item := Notification{Seq: n.seq, RoomID: room.ID, RoomName: room.Name, HostMode: room.HostMode, Kind: kind, Slot: slot, At: now}
	n.items = append(n.items, item)
	if len(n.items) > notificationHistory {
		n.items = append([]Notification(nil), n.items[len(n.items)-notificationHistory:]...)
	}
	if len(n.seen) > 8*notificationHistory {
		n.seen = map[string]bool{dedupe: true}
	}
	close(n.changed)
	n.changed = make(chan struct{})
	n.mu.Unlock()
	if len(n.cfg.Command) > 0 {
		select {
		case n.queue <- item:
		default: // A stuck command must not grow memory or block a Room.
		}
	}
	return true
}

// Since returns notifications newer than seq, oldest first, and a channel that
// closes on the next change.
func (n *Notifier) Since(seq uint64) ([]Notification, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Notification
	for _, item := range n.items {
		if item.Seq > seq {
			out = append(out, item)
		}
	}
	return out, n.changed
}

func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.queue)
	n.mu.Unlock()
	<-n.done
}

func (n *Notifier) deliver() {
	defer close(n.done)
	for item := range n.queue {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), notifyCommandTimeout)
		_ = n.cfg.Run(ctx, n.cfg.Command, data)
		cancel()
	}
}

func runNotifyCommand(ctx context.Context, argv []string, stdin []byte) error {
	if len(argv) == 0 {
		return errors.New("notify command is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Run()
}

// readNotifications returns body-free notifications newer than ?after=<seq>,
// oldest first. With ?wait=<1-30> seconds it long-polls until one arrives.
// Reading never acknowledges, clears or changes any Room state.
func (s *ManagementServer) readNotifications(w http.ResponseWriter, r *http.Request) {
	var after uint64
	if value := r.URL.Query().Get("after"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeManagementError(w, http.StatusBadRequest, "after must be a notification sequence")
			return
		}
		after = parsed
	}
	wait := 0
	if value := r.URL.Query().Get("wait"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 30 {
			writeManagementError(w, http.StatusBadRequest, "wait must be 0-30 seconds")
			return
		}
		wait = parsed
	}
	items := []Notification{}
	if s.notifier != nil {
		found, changed := s.notifier.Since(after)
		if len(found) == 0 && wait > 0 {
			timer := time.NewTimer(time.Duration(wait) * time.Second)
			select {
			case <-changed:
				found, _ = s.notifier.Since(after)
			case <-timer.C:
			case <-r.Context().Done():
			case <-s.streams.Done():
			}
			timer.Stop()
		}
		if found != nil {
			items = found
		}
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{"notifications": items})
}
