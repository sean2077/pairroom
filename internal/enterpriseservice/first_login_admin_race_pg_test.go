package enterpriseservice

import (
	"io"
	"net/http"
	"sync"
	"testing"
)

// The operator-selected identity is the bootstrap authority. Concurrent first
// OIDC callbacks from admitted members or unadmitted accounts cannot claim it.
func TestFirstLoginAdministratorConcurrentNonBootstrapAccountsHTTPPG(t *testing.T) {
	f := newHTTPPGFixture(t)
	subjects := []string{"member-one", "member-two", "member-one", "member-two", "unadmitted-one", "unadmitted-two"}
	clients := make([]*http.Client, len(subjects))
	for i, subject := range subjects {
		clients[i] = f.browser(subject)
	}
	type loginResult struct {
		status int
		err    error
	}
	results := make([]loginResult, len(clients))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(i int, client *http.Client) {
			defer wg.Done()
			<-start
			response, err := client.Get(f.origin + "/enterprise/auth/login")
			if err != nil {
				results[i].err = err
				return
			}
			results[i].status = response.StatusCode
			_, results[i].err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}(i, client)
	}
	close(start)
	wg.Wait()
	for i, result := range results {
		want := http.StatusOK
		if i >= 4 {
			want = http.StatusForbidden
		}
		if result.err != nil || result.status != want {
			t.Fatalf("first-login cohort %d: status=%d want=%d error=%v", i, result.status, want, result.err)
		}
		if i < 4 && f.session(clients[i]).Administrator {
			t.Fatal("concurrent non-bootstrap login obtained administrator authority")
		}
	}
	var administrators, unauthorizedIdentities int
	var consumed string
	if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM organization_admins WHERE enabled),COALESCE(bootstrap_consumed_by,''),(SELECT count(*) FROM external_identities WHERE subject IN ('unadmitted-one','unadmitted-two')) FROM identity_policy WHERE id`).Scan(&administrators, &consumed, &unauthorizedIdentities); err != nil {
		t.Fatal(err)
	}
	if administrators != 0 || consumed != "" || unauthorizedIdentities != 0 {
		t.Fatal("first-login race consumed bootstrap, granted administration, or admitted an unauthorized identity")
	}
	operator := f.browser("operator")
	f.login(operator)
	selected := f.session(operator)
	if !selected.Administrator {
		t.Fatal("explicitly selected bootstrap identity cannot obtain its own authority")
	}
	if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM organization_admins WHERE enabled),COALESCE(bootstrap_consumed_by,'') FROM identity_policy WHERE id`).Scan(&administrators, &consumed); err != nil {
		t.Fatal(err)
	}
	if administrators != 1 || consumed != selected.UserID {
		t.Fatal("bootstrap was not consumed exactly by the selected operator")
	}
	for i := 0; i < 4; i++ {
		if f.session(clients[i]).Administrator {
			t.Fatal("selected operator login promoted an earlier member session")
		}
	}
}
