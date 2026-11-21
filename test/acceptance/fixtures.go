//go:build acceptance

package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/proshik/krill/internal/dbservice/drivers"
)

// The state-backup acceptance's own services in the VM. None of the names
// contains "krill", so the leftover checks can grep for that word; both keep
// their data in bind mounts, never in Docker volumes, so wiping Krill's
// volumes cannot touch them. They play the operator's external storage and
// registry and survive scenario A's wipe.
const (
	s3Container   = "accept-s3"
	s3Data        = "/srv/accept-s3"
	s3EnvFile     = "/root/accept-s3.env"
	s3Endpoint    = "http://127.0.0.1:9000"
	stateBucket   = "krill-state"
	statePrefix   = "accept"
	appBucket     = "app-backups"
	statePolicy   = "krill-state-backup"
	regContainer  = "accept-registry"
	regData       = "/srv/accept-registry"
	regPassFile   = "/root/accept-registry.pw"
	s3UserFile    = "/root/accept-s3-user.secret"
	ldbPassFile   = "/root/accept-ldb.pw"
	registryImage = "registry:2.8.3"
	registryHost  = "127.0.0.1:5000"
	appRepo       = registryHost + "/accept/web"
	appTag        = "1"
	appSrcImage   = "nginx:1.27-alpine"
	// restoreDir holds the backup object and, during a restore only, the age
	// private key, as an operator's working directory would.
	restoreDir = "/root/accept-restore"
)

// minioImage is the image Krill's own S3 engine uses: anonymous pull,
// multi-arch, ships mc and curl.
var minioImage = drivers.MinIOImage

// Fixtures are the random credentials of one run's storage and registry.
type Fixtures struct {
	S3RootUser, S3RootPassword string
	S3User, S3Secret           string // the state backup's restricted user
	RegUser, RegPassword       string
}

// NewFixtures generates fresh credentials and registers every secret for
// redaction.
func NewFixtures() Fixtures {
	f := Fixtures{
		S3RootUser: "acceptroot", S3RootPassword: randHex(20),
		S3User: "acceptstate", S3Secret: randHex(20),
		RegUser: "accept", RegPassword: randHex(16),
	}
	for _, v := range []string{f.S3RootPassword, f.S3Secret, f.RegPassword} {
		redactValue(v)
	}
	return f
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// S3EnvFile is the --env-file of the MinIO container: the root credentials,
// and an mc alias "s3" so `docker exec accept-s3 mc ...` needs no secret on
// its command line.
func (f Fixtures) S3EnvFile() string {
	return fmt.Sprintf("MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\nMC_HOST_s3=http://%s:%s@127.0.0.1:9000\n",
		f.S3RootUser, f.S3RootPassword, f.S3RootUser, f.S3RootPassword)
}

// StatePolicyJSON grants exactly the permissions docs/guides/krill-backups.md
// lists for the state backup, on the state bucket only.
func StatePolicyJSON() string {
	return `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::` + stateBucket + `"]},` +
		`{"Effect":"Allow","Action":["s3:PutObject","s3:AbortMultipartUpload","s3:GetObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + stateBucket + `/*"]}]}`
}

// removeFixturesScript removes both services, their data and the restore
// directory (idempotent).
const removeFixturesScript = "docker rm -f " + s3Container + " " + regContainer + " krill-restore-test >/dev/null 2>&1 || true\n" +
	"rm -rf " + s3Data + " " + regData + " " + s3EnvFile + " " + regPassFile + " " + s3UserFile + " " + ldbPassFile + " " + restoreDir +
	" /root/accept-env-edit.sh"

// StartScript starts MinIO and the registry from files already staged as
// root (s3EnvFile, regPassFile, s3UserFile, <regData>/auth/htpasswd,
// /tmp/krill-accept/state-policy.json), creates the buckets, the policy and the restricted
// user, and pushes the app image into the registry.
func (f Fixtures) StartScript() string {
	return strings.Join([]string{
		"set -e",
		"mkdir -p " + s3Data,
		"docker run -d --name " + s3Container + " --restart unless-stopped --env-file " + s3EnvFile +
			" -p 127.0.0.1:9000:9000 -v " + s3Data + ":/data " + minioImage + " server /data",
		"for i in $(seq 60); do docker exec " + s3Container + " curl -fsS http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1 && break; sleep 1; done",
		"docker exec " + s3Container + " curl -fsS http://127.0.0.1:9000/minio/health/live",
		"docker exec " + s3Container + " mc mb --ignore-existing s3/" + stateBucket + " s3/" + appBucket,
		"docker exec -i " + s3Container + " sh -c 'cat > /tmp/state-policy.json' < /tmp/krill-accept/state-policy.json",
		"docker exec " + s3Container + " mc admin policy create s3 " + statePolicy + " /tmp/state-policy.json",
		// The secret goes in through a file, never on a command line here.
		"docker exec -i " + s3Container + " sh -c 'cat > /tmp/user.secret' < " + s3UserFile,
		"docker exec " + s3Container + " sh -c 'mc admin user add s3 " + f.S3User + " \"$(cat /tmp/user.secret)\"; rc=$?; rm -f /tmp/user.secret; exit $rc'",
		"docker exec " + s3Container + " mc admin policy attach s3 " + statePolicy + " --user " + f.S3User,
		"docker run -d --name " + regContainer + " --restart unless-stopped -p 127.0.0.1:5000:5000" +
			" -v " + regData + "/data:/var/lib/registry -v " + regData + "/auth:/auth" +
			" -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=accept -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd " + registryImage,
		"for i in $(seq 60); do curl -s -o /dev/null http://" + registryHost + "/v2/ && break; sleep 1; done",
		"docker pull " + appSrcImage,
		"docker tag " + appSrcImage + " " + appRepo + ":" + appTag,
		"docker login " + registryHost + " -u " + f.RegUser + " --password-stdin < " + regPassFile,
		"docker push " + appRepo + ":" + appTag,
		"docker logout " + registryHost,
		"echo fixtures-ready",
	}, "\n")
}
