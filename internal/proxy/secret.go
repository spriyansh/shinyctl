package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spriyansh/shinyctl/internal/atomicfile"
)

// secretForm is what a key looks like: 32 random bytes in hex.
var secretForm = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadSecret returns the key of the cookies that keep visitors on their
// copy: the contents of dir/secret, created with 32 random bytes (64 hex
// characters, mode 0600) when the file is missing. One key serves the whole
// installation. A file that can't be read, or doesn't hold a key, is not a
// reason for shinyctl-api to stay down: it is replaced, atomically, and the
// only cost is that every visitor is placed on a copy once more.
func LoadSecret(dir string) (string, error) {
	path := filepath.Join(dir, "secret")
	data, err := os.ReadFile(path)
	if err == nil {
		if secret := strings.TrimSpace(string(data)); secretForm.MatchString(secret) {
			return secret, nil
		}
		err = fmt.Errorf("it does not hold a key")
	}
	unusable := !os.IsNotExist(err) // a file that is there but of no use, not a fresh install

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(key)
	if werr := atomicfile.Write(path, []byte(secret+"\n")); werr != nil {
		if unusable {
			return "", fmt.Errorf("%s is unusable (%v) and a new key cannot be written: %w", path, err, werr)
		}
		return "", werr
	}
	if unusable {
		log.Printf("proxy: the key for the load balancer's cookies was unusable (%v); made a new one; visitors pick a copy again once", err)
	}
	return secret, nil
}
