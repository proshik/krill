package envclone

import (
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/envtext"
)

func TestCopyKeepsPublicTextAndBlanksAllSecretAndLinkedOccurrences(t *testing.T) {
	raw := "# TOKEN=comment\r\n PUBLIC =  literal=https://production/  \r\nSECRET=old=secret\r\n SECRET =another\r\nDB=production-dsn\r\n\r\ninvalid\r\n"
	want := "# TOKEN=comment\r\n PUBLIC =  literal=https://production/  \r\nSECRET=\r\n SECRET =\r\nDB=\r\n\r\ninvalid\r\n"
	if got := envtext.Clone(raw, []string{"PUBLIC", "DB"}, []string{"DB"}); got != want {
		t.Fatalf("clone did not preserve formatting or clear every secret")
	}
}

func TestCopyApplicationResetsIdentityAndPreservesRuntime(t *testing.T) {
	memory, cpu, command, hc := "256m", "0.5", "serve", "check"
	oldToken := "old-token"
	src := db.Application{
		ID: 10, EnvironmentID: 2, Name: "web", Domain: "production.test", Status: "running",
		Image: "image", Tag: "tag", Port: 8080, SourceType: "dockerfile",
		GitUrl: "https://user:secret@git.test/a.git?service=git-upload-pack#branch", GitBranch: "main", DockerfilePath: "Dockerfile",
		BuildArgs: "PUBLIC=production", BuildSecrets: "PRIVATE=test", AutoDeploy: true, WebhookSecret: "secret",
		MemoryLimit: &memory, CpuLimit: &cpu, Replicas: 3, Command: &command, PredeployCommand: "migrate",
		RestartCondition: "on-failure", RestartMaxAttempts: 5, HealthcheckCmd: &hc,
		PlacementMode: "pin", PlacementNodes: "node-1", MetricsEnabled: true, MetricsToken: &oldToken,
		MetricsTokenEnv: "TOKEN", EnvText: "DB=old\nPUBLIC=value", EnvPublicKeys: []string{"DB", "PUBLIC", "STALE"},
	}
	out, err := CopyApplication(src, []db.ListDBLinksByApplicationRow{{VarName: "DB"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != 0 || out.EnvironmentID != 0 || out.Status != "idle" || out.Domain != "" || out.AutoDeploy || out.WebhookSecret != "" || out.BuildSecrets != "" {
		t.Fatal("copied source identity or build secrets")
	}
	if out.GitUrl != "https://git.test/a.git?service=git-upload-pack#branch" || out.EnvText != "DB=\nPUBLIC=value" || strings.Join(out.EnvPublicKeys, ",") != "DB,PUBLIC" {
		t.Fatal("incorrect provenance or env sanitization")
	}
	if out.MetricsToken == nil || *out.MetricsToken == oldToken || !out.MetricsEnabled || out.MetricsTokenEnv != "TOKEN" {
		t.Fatal("metrics identity was not regenerated")
	}
	if out.PredeployCommand != "migrate" || out.Command != src.Command || out.MemoryLimit != src.MemoryLimit || out.CpuLimit != src.CpuLimit || out.Replicas != 3 || out.RestartMaxAttempts != 5 || out.HealthcheckCmd != src.HealthcheckCmd || out.PlacementNodes != src.PlacementNodes || out.BuildArgs != src.BuildArgs {
		t.Fatal("runtime configuration was lost")
	}
	if src.EnvText != "DB=old\nPUBLIC=value" || src.GitUrl == out.GitUrl || *src.MetricsToken != oldToken {
		t.Fatal("source was mutated")
	}
	inst := int64(4)
	if _, err := CopyApplication(src, []db.ListDBLinksByApplicationRow{{InstanceID: &inst}}); err != nil {
		t.Fatal(err)
	}
	src.GitUrl = "https://git.test/a.git?token=secret"
	if _, err := CopyApplication(src, nil); err == nil {
		t.Fatal("credential-bearing query silently copied")
	}

}

func TestNamesKeepIdentityWhenPrefixesAreLong(t *testing.T) {
	suffix := strings.Repeat("a", 32)
	name, err := DatabaseName(strings.Repeat("very_long_", 20), suffix)
	if err != nil || len(name) != 63 || !strings.HasSuffix(name, "_"+suffix) {
		t.Fatalf("database name lost suffix: %q %v", name, err)
	}
	for _, id := range []int64{1, 2, 9223372036854775807} {
		host, err := PrimaryHost(strings.Repeat("web-", 30), id, "local.test")
		if err != nil || len(strings.Split(host, ".")[0]) > 63 || !dnsLabelPattern.MatchString(strings.Split(host, ".")[0]) {
			t.Fatalf("invalid generated host %q: %v", host, err)
		}
	}
	if _, err := PrimaryHost("web", 1, "bad..test"); err == nil {
		t.Fatal("accepted invalid domain")
	}
	if _, err := PrimaryHost("web", 1, strings.Repeat("a", 63)+"."+strings.Repeat("b", 63)+"."+strings.Repeat("c", 63)+"."+strings.Repeat("d", 63)); err == nil {
		t.Fatal("accepted host exceeding total DNS length")
	}
}
