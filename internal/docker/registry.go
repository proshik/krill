package docker

import (
	"encoding/base64"
	"encoding/json"

	"github.com/docker/docker/api/types/registry"
)

// EncodeRegistryAuth returns the base64url(JSON) auth blob Swarm expects in
// EncodedRegistryAuth for pulling a private image from serverAddr.
func EncodeRegistryAuth(username, password, serverAddr string) (string, error) {
	b, err := json.Marshal(registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: serverAddr,
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
