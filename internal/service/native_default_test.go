package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestManagementNewRoomsDefaultToNativeWithoutProvisioningAdapters(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	provisioned := 0
	provisioner := ProvisionerFunc(func(ctx context.Context, project Project, slot model.ActorID, spec BindingSpec, dir string) (Binding, func(context.Context) error, error) {
		provisioned++
		return (SyntheticProvisioner{}).Provision(ctx, project, slot, spec, dir)
	})
	server, _ := newManagementTestServer(t, registry, provisioner)
	for _, tc := range []struct {
		name string
		body string
		mode model.HostMode
		want int
	}{
		{"omitted", `{"name":"default native"}`, model.HostNative, 0},
		{"empty", `{"name":"empty native","host_mode":""}`, model.HostNative, 0},
		{"embedded", `{"name":"explicit embedded","host_mode":"embedded","bindings":{"slot1":{"mode":"new"},"slot2":{"mode":"new"}}}`, model.HostEmbedded, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := provisioned
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", tc.body, true))
			if response.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
			}
			var created Room
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.HostMode != tc.mode || provisioned-before != tc.want {
				t.Fatalf("mode=%q provisioned=%d; want mode=%q provisioned=%d", created.HostMode, provisioned-before, tc.mode, tc.want)
			}
			if tc.mode == model.HostNative {
				for _, binding := range created.Bindings {
					if !binding.Pending || binding.SessionID != "" {
						t.Fatalf("default creation fabricated a native session: %+v", binding)
					}
				}
			}
		})
	}
}
