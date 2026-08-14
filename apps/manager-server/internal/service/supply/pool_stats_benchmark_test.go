package supply

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func BenchmarkAccountPoolStatsFromFiles(b *testing.B) {
	for _, size := range []int{121, 500, 1000} {
		files, _ := benchmarkPoolFixture(size)
		b.Run(fmt.Sprintf("accounts_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = accountPoolStatsFromFiles(files)
			}
		})
	}
}

func BenchmarkAccountPoolStatsFromFilesAndInspection(b *testing.B) {
	for _, size := range []int{121, 500, 1000} {
		files, results := benchmarkPoolFixture(size)
		b.Run(fmt.Sprintf("accounts_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = accountPoolStatsFromFilesAndInspection(files, results)
			}
		})
	}
}

func BenchmarkAccountPoolStatsFromFilesAndCurrentEvidence(b *testing.B) {
	for _, size := range []int{121, 500, 1000} {
		files, results := benchmarkPoolFixture(size)
		headers := make([]store.HeaderSnapshot, 0, size)
		used := 90.0
		for _, file := range files {
			headers = append(headers, store.HeaderSnapshot{
				AuthFileSnapshot: file.Name,
				AuthIndex:        file.AuthIndex,
				TimestampMS:      1_000,
				ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
					Primary: &usage.HeaderQuotaWindow{UsedPercent: &used},
				}},
			})
		}
		b.Run(fmt.Sprintf("accounts_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = accountPoolStatsFromFilesAndCurrentEvidence(
					files,
					results,
					headers,
					model.CodexInspectionTriggerSupplySnapshot,
					time.UnixMilli(2_000),
				)
			}
		})
	}
}

func BenchmarkBuildSmartResourceFromInspectionSnapshot(b *testing.B) {
	for _, size := range []int{121, 500, 1000} {
		_, results := benchmarkPoolFixture(size)
		now := time.Now().Truncate(time.Second)
		snapshot := inspectionQuotaSnapshot{
			run: store.CodexInspectionRun{
				ID:            1,
				ProbeSetCount: size,
				SampledCount:  size,
				FinishedAtMS:  now.UnixMilli(),
			},
			results:     results,
			generatedAt: now,
		}
		service := New(nil, nil)
		cfg := store.ManagerSupplyConfig{Product: "oauth_7d", HealthyMinutesTarget: 60}
		b.Run(fmt.Sprintf("accounts_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = service.buildSmartResourceFromInspectionSnapshot(cfg, snapshot, now)
			}
		})
	}
}

func BenchmarkBuildSmartResourceFromInspectionSnapshotWithQuotaSamples(b *testing.B) {
	for _, size := range []int{121, 500, 1000} {
		_, results := benchmarkPoolFixture(size)
		now := time.Now().Truncate(time.Second)
		snapshot := inspectionQuotaSnapshot{
			run: store.CodexInspectionRun{
				ID:            1,
				ProbeSetCount: size,
				SampledCount:  size,
				FinishedAtMS:  now.UnixMilli(),
			},
			results:     results,
			generatedAt: now,
		}
		service := New(nil, nil)
		for index, result := range results {
			if result.Disabled {
				continue
			}
			service.smartQuotaState.samples = append(
				service.smartQuotaState.samples,
				quotaSamplesForEstimate(
					"file:"+result.FileName,
					"team",
					55+float64(index%5),
					now.Add(-time.Minute),
					6,
				)...,
			)
		}
		cfg := store.ManagerSupplyConfig{Product: "oauth_7d", HealthyMinutesTarget: 60}
		b.Run(fmt.Sprintf("accounts_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = service.buildSmartResourceFromInspectionSnapshot(cfg, snapshot, now)
			}
		})
	}
}

func BenchmarkGetStatusWithCPAFixture(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(benchmarkAuthFilesPayload(500)))
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(b.TempDir(), "get-status.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = st.Close() })
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "benchmark"},
		Supply: store.ManagerSupplyConfig{
			Product: "oauth_7d", SmartEnabled: &smartDisabled, AuthFilesCacheTTLSeconds: 60,
		},
	}); err != nil {
		b.Fatal(err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if _, err := service.GetStatus(context.Background(), 50); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := service.GetStatus(context.Background(), 50); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkAuthFilesPayload(size int) string {
	var builder strings.Builder
	builder.WriteString(`{"files":[`)
	for index := 0; index < size; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"name":"account-%04d.json","provider":"codex","disabled":false,"status":"ready"}`, index)
	}
	builder.WriteString(`]}`)
	return builder.String()
}

func benchmarkPoolFixture(size int) ([]cpaauthfiles.File, []store.CodexInspectionResult) {
	files := make([]cpaauthfiles.File, 0, size)
	results := make([]store.CodexInspectionResult, 0, size)
	used := 10.0
	for index := 0; index < size; index++ {
		name := fmt.Sprintf("account-%04d.json", index)
		authIndex := fmt.Sprintf("auth-%04d", index)
		disabled := index%10 == 0
		concurrency := any(0)
		switch index % 4 {
		case 0:
			concurrency = 1
		case 1:
			concurrency = 2
		case 2:
			concurrency = 0
		default:
			concurrency = nil
		}
		raw := map[string]any{"status": "active"}
		if concurrency != nil {
			raw["max_concurrency"] = concurrency
		}
		files = append(files, cpaauthfiles.File{
			Name:      name,
			Provider:  "codex",
			AuthIndex: authIndex,
			Disabled:  disabled,
			Raw:       raw,
		})
		results = append(results, store.CodexInspectionResult{
			FileName:    name,
			Provider:    "codex",
			AuthIndex:   authIndex,
			Action:      "keep",
			Status:      "active",
			Disabled:    disabled,
			UsedPercent: &used,
		})
	}
	return files, results
}
