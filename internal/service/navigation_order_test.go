package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func navigationFixture(t *testing.T) (*Registry, Project, []Room) {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	rooms := []Room{}
	for _, name := range []string{"Zebra", "Alpha", "Middle"} {
		room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: name, Bindings: specs(BindingNew, BindingNew, name)}, SyntheticProvisioner{})
		if err != nil {
			t.Fatal(err)
		}
		rooms = append(rooms, room)
	}
	return registry, project, rooms
}

func TestNavigationOrderPersistenceAndIsolation(t *testing.T) {
	registry, project, rooms := navigationFixture(t)
	before := registry.Snapshot(true)
	initial, err := registry.NavigationOrder()
	if err != nil || len(initial.Projects) != 0 || initial.Schema != 1 {
		t.Fatalf("initial order: %+v %v", initial, err)
	}
	order, err := registry.MoveNavigation(context.Background(), NavigationMove{"room", rooms[2].ID, rooms[0].ID, "before"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{rooms[2].ID, rooms[0].ID, rooms[1].ID}
	if !reflect.DeepEqual(order.Rooms[project.ID], want) {
		t.Fatalf("order: %+v", order)
	}
	after := registry.Snapshot(true)
	if !reflect.DeepEqual(before.Rooms, after.Rooms) || !reflect.DeepEqual(before.Projects, after.Projects) {
		t.Fatal("display move changed Room/Project facts")
	}
	// Rebuilding the Registry from durable Room state must retain user ordering.
	if err := os.Remove(registry.checkpoint); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.NavigationOrder()
	if err != nil || !reflect.DeepEqual(order, got) {
		t.Fatalf("restart/rebuild lost order: %+v %v", got, err)
	}
	// A newly created Room is appended, and an unrelated single-item move keeps
	// the order of all hidden items. This also covers a stale client's request.
	extra, err := reopened.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "New", Bindings: specs(BindingNew, BindingNew, "new")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	got, err = reopened.MoveNavigation(context.Background(), NavigationMove{"room", rooms[1].ID, rooms[0].ID, "before"})
	want = []string{rooms[2].ID, rooms[1].ID, rooms[0].ID, extra.ID}
	if err != nil || !reflect.DeepEqual(got.Rooms[project.ID], want) {
		t.Fatalf("move lost hidden/new items: %+v %v", got, err)
	}
	// Reads are owned values: callers cannot mutate later reports in memory.
	got.Rooms[project.ID][0] = "not-a-room"
	next, _ := reopened.NavigationOrder()
	if !reflect.DeepEqual(next.Rooms[project.ID], want) {
		t.Fatal("aliased order")
	}
}

func TestNavigationOrderValidationAndFailures(t *testing.T) {
	registry, _, rooms := navigationFixture(t)
	other, err := registry.RegisterProject(context.Background(), testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: other.ID, Name: "Foreign", Bindings: specs(BindingNew, BindingNew, "foreign")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	for _, move := range []NavigationMove{
		{"room", rooms[0].ID, foreign.ID, "before"}, {"room", "missing", rooms[0].ID, "before"},
		{"room", rooms[0].ID, rooms[0].ID, "after"}, {"project", other.ID, "missing", "before"},
		{"room", rooms[0].ID, rooms[1].ID, "first"}, {"unknown", rooms[0].ID, rooms[1].ID, "before"},
	} {
		if _, err := registry.MoveNavigation(context.Background(), move); !errors.Is(err, errNavigationOrderMove) {
			t.Fatalf("accepted invalid move %+v: %v", move, err)
		}
	}
	move := NavigationMove{"room", rooms[1].ID, rooms[0].ID, "before"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.MoveNavigation(ctx, move); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	path := filepath.Join(registry.Root(), navigationOrderFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected/cancelled move wrote preferences")
	}
	for _, content := range []string{"null", "{}", `{"schema":99,"projects":[],"rooms":{}}`, `{"schema":1,"projects":["duplicate","duplicate"],"rooms":{}}`, `{"schema":1,"projects":[],"rooms":{}} {}`, `{"schema":1,"projects":[],"rooms":{},"unknown":true}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.MoveNavigation(context.Background(), move); !errors.Is(err, errNavigationOrderStore) {
			t.Fatalf("overwrote malformed preferences %s: %v", content, err)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != content {
			t.Fatal("failed write changed preferences")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MoveNavigation(context.Background(), move); !errors.Is(err, errNavigationOrderStore) {
		t.Fatal("overwrote non-file order")
	}
}

func TestNavigationOrderConcurrentMoves(t *testing.T) {
	registry, project, rooms := navigationFixture(t)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, err := registry.MoveNavigation(context.Background(), NavigationMove{"room", rooms[i%2].ID, rooms[2].ID, "before"})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	group.Wait()
	order, err := registry.NavigationOrder()
	if err != nil || len(order.Rooms[project.ID]) != 3 || !validOrderIDs(order.Rooms[project.ID]) {
		t.Fatalf("concurrent move corrupted order: %+v %v", order, err)
	}
}

func TestManagementNavigationOrder(t *testing.T) {
	registry, project, rooms := navigationFixture(t)
	server, manager := newManagementTestServer(t, registry, SyntheticProvisioner{})
	body, _ := json.Marshal(NavigationMove{"room", rooms[1].ID, rooms[0].ID, "before"})
	for _, authenticated := range []bool{false, true} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(http.MethodPatch, navigationOrderPath, string(body), authenticated))
		expected := 401
		if authenticated {
			expected = 200
		}
		if response.Code != expected {
			t.Fatalf("PATCH %d: %s", response.Code, response.Body.String())
		}
	}
	bootstrap := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrap, managementRequest("POST", "/api/v1/session", "", true))
	request := managementRequest("PATCH", navigationOrderPath, string(body), false)
	for _, cookie := range bootstrap.Result().Cookies() {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("missing CSRF protection")
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("PATCH", navigationOrderPath, `{"unexpected":true}`, true))
	if response.Code != 400 {
		t.Fatal("unknown JSON fields accepted")
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("GET", "/api/v1/service", "", true))
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.NavigationOrder == nil || snapshot.NavigationOrder.Rooms[project.ID][0] != rooms[1].ID || len(manager.Statuses()) != 0 {
		t.Fatal("order missing or runtime activated")
	}
	if err := os.WriteFile(filepath.Join(registry.Root(), navigationOrderFile), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("GET", "/api/v1/service", "", true))
	snapshot = ServiceSnapshot{}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || !snapshot.NavigationOrderError || !snapshot.Healthy {
		t.Fatal("corrupt UI preferences broke Service health or were hidden")
	}
}

func TestNavigationOrderProjectsAndLifecycle(t *testing.T) {
	registry, first, rooms := navigationFixture(t)
	ctx := context.Background()
	second, err := registry.RegisterProject(ctx, testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	third, err := registry.RegisterProject(ctx, testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	order, err := registry.MoveNavigation(ctx, NavigationMove{"project", third.ID, first.ID, "before"})
	if err != nil || !reflect.DeepEqual(order.Projects, []string{third.ID, first.ID, second.ID}) {
		t.Fatalf("project move: %+v %v", order, err)
	}
	if _, err := registry.RemoveProject(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	order, err = registry.MoveNavigation(ctx, NavigationMove{"project", third.ID, first.ID, "after"})
	if err != nil || !reflect.DeepEqual(order.Projects, []string{first.ID, third.ID}) {
		t.Fatalf("stale deleted ID: %+v %v", order, err)
	}
	if _, err := registry.ArchiveRoom(ctx, rooms[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MoveNavigation(ctx, NavigationMove{"room", rooms[0].ID, rooms[1].ID, "before"}); !errors.Is(err, errNavigationOrderMove) {
		t.Fatalf("cross-lifecycle move accepted: %v", err)
	}
	if _, err := registry.ArchiveRoom(ctx, rooms[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MoveNavigation(ctx, NavigationMove{"room", rooms[1].ID, rooms[0].ID, "before"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	order, err = reopened.NavigationOrder()
	if err != nil || !reflect.DeepEqual(order.Projects, []string{first.ID, third.ID}) || order.Rooms[first.ID][0] != rooms[1].ID {
		t.Fatalf("project/archived restart: %+v %v", order, err)
	}
}

func TestNavigationOrderRejectsSymlinkAndOversize(t *testing.T) {
	registry, _, rooms := navigationFixture(t)
	path := filepath.Join(registry.Root(), navigationOrderFile)
	// A failed destination must not leave optimistic values or temp files.
	if err := os.WriteFile(path, make([]byte, maxNavigationOrderBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	move := NavigationMove{"room", rooms[1].ID, rooms[0].ID, "before"}
	if _, err := registry.MoveNavigation(context.Background(), move); !errors.Is(err, errNavigationOrderStore) {
		t.Fatal("oversize accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	original := `{"schema":1,"projects":[],"rooms":{}}`
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := registry.MoveNavigation(context.Background(), move); !errors.Is(err, errNavigationOrderStore) {
		t.Fatal("symlink accepted")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != original {
		t.Fatal("overwrote symlink target")
	}
	matches, _ := filepath.Glob(filepath.Join(registry.Root(), ".navigation-order-*.tmp"))
	if len(matches) != 0 {
		t.Fatal("leaked temporary preference files")
	}
}
