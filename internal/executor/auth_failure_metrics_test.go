package executor

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	hyperfleetlogger "github.com/openshift-hyperfleet/hyperfleet-logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
)

func TestExecutor_PostActionAuthFailureLogsAreContextualAndRedacted(t *testing.T) {
	tests := []struct {
		sentinel           string
		name               string
		resourceID         string
		kind               string
		ownerID            string
		ownerKind          string
		statusCode         int
		expectAuthLog      bool
		expectResponseBody bool
	}{
		{
			name:          "401 unauthorized",
			resourceID:    "cluster-auth-401",
			kind:          "Cluster",
			statusCode:    http.StatusUnauthorized,
			sentinel:      "qe-auth-response-body-401-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:          "403 forbidden",
			resourceID:    "nodepool-auth-403",
			kind:          "NodePool",
			statusCode:    http.StatusForbidden,
			sentinel:      "qe-auth-response-body-403-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:          "403 forbidden for a nodepool owned by a cluster",
			resourceID:    "nodepool-owned-auth-403",
			kind:          "NodePool",
			ownerKind:     "Cluster",
			ownerID:       "cluster-owner-nodepool-auth-403",
			statusCode:    http.StatusForbidden,
			sentinel:      "qe-owned-nodepool-auth-response-body-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:          "401 unauthorized for a non-cluster resource",
			resourceID:    "database-auth-401",
			kind:          "Database",
			statusCode:    http.StatusUnauthorized,
			sentinel:      "qe-database-auth-response-body-must-not-be-logged",
			expectAuthLog: true,
		},
		{
			name:               "500 server error",
			resourceID:         "database-server-error",
			kind:               "Database",
			statusCode:         http.StatusInternalServerError,
			sentinel:           "qe-non-auth-response-body",
			expectResponseBody: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := slog.Default()
			var logs bytes.Buffer
			slog.SetDefault(slog.New(hyperfleetlogger.NewHandler(
				"test",
				"test",
				hyperfleetlogger.WithLevel(slog.LevelDebug),
				hyperfleetlogger.WithFormat(hyperfleetlogger.FormatText),
				hyperfleetlogger.WithOutput(&logs),
				hyperfleetlogger.WithContextFields(logctx.ContextFields()...),
			)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			mockClient := newMockAPIClient()
			mockClient.GetResponse = &hyperfleetapi.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       []byte(`{}`),
			}
			mockClient.PutResponse = &hyperfleetapi.Response{
				StatusCode: tt.statusCode,
				Status:     fmt.Sprintf("%d %s", tt.statusCode, http.StatusText(tt.statusCode)),
				Body:       []byte(tt.sentinel),
			}

			exec, err := NewBuilder().
				WithConfig(new404PostActionConfig()).
				WithAPIClient(mockClient).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			require.NoError(t, err)

			eventData := map[string]interface{}{
				"id":   tt.resourceID,
				"kind": tt.kind,
			}
			if tt.ownerKind != "" {
				eventData["owner_references"] = map[string]interface{}{
					"kind": tt.ownerKind,
					"id":   tt.ownerID,
				}
			}
			result := exec.Execute(t.Context(), eventData)
			require.Equal(t, StatusFailed, result.Status, "post-action API failure must fail the execution")

			captured := logs.String()
			executionMessage := result.ExecutionContext.Adapter.ExecutionError.Message
			require.Len(t, result.PostActionResults, 1)
			postActionResponse := string(result.PostActionResults[0].APIResponse)
			if tt.expectResponseBody {
				assert.Contains(t, captured, tt.sentinel,
					"non-auth response details must remain available in executor logs")
				assert.Contains(t, executionMessage, tt.sentinel,
					"non-auth response details must remain available to status conditions")
				assert.Contains(t, postActionResponse, tt.sentinel,
					"non-auth response details must remain available in post-action results")
			} else {
				assert.NotContains(t, captured, tt.sentinel,
					"auth response bodies must not be written to executor logs")
				assert.NotContains(t, executionMessage, tt.sentinel,
					"auth response bodies must not reach status conditions")
				assert.NotContains(t, postActionResponse, tt.sentinel,
					"auth response bodies must not remain in post-action results")
			}
			if !tt.expectAuthLog {
				assert.NotContains(t, captured, "http_status=500",
					"non-auth API failures must not emit the dedicated auth-failure log")
				return
			}

			assert.Contains(t, captured, "ERROR")
			assert.Contains(t, captured, fmt.Sprintf("http_status=%d", tt.statusCode))
			assert.Contains(t, captured, "post_action=reportStatus")
			assert.NotContains(t, captured, "cluster_id=")
			matchedAuthLog := false
			for line := range strings.SplitSeq(captured, "\n") {
				if strings.Contains(line, "hyperfleet api authentication or authorization failed") {
					matchedAuthLog = true
					assert.Contains(t, line, " resource_id="+tt.resourceID)
					assert.Contains(t, line, "kind="+tt.kind)
					assert.Equal(t, 1, strings.Count(line, " resource_id="), "resource_id must not be duplicated")
					if tt.ownerKind != "" {
						assert.Contains(t, line, " resource_type="+tt.kind)
						assert.Contains(t, line, " owner_resource_type="+tt.ownerKind)
						assert.Contains(t, line, " owner_resource_id="+tt.ownerID)
					}
				}
			}
			assert.True(t, matchedAuthLog, "expected the auth-failure log line")
		})
	}
}
