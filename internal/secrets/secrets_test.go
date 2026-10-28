package secrets

import (
	"strings"
	"testing"
)

// The positives are built at run time: GitHub push protection rejects
// literals shaped like real credentials.
func TestScan(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		found   bool
	}{
		{"rsa private key", "-----BEGIN " + "RSA PRIVATE KEY-----", "private-key", true},
		{"openssh private key", "-----BEGIN " + "OPENSSH PRIVATE KEY-----", "private-key", true},
		{"aws access key", "AKIA" + strings.Repeat("Q", 16), "aws-access-key", true},
		{"aws session key", "ASIA" + strings.Repeat("Q", 16), "aws-access-key", true},
		{"github token", "ghp_" + strings.Repeat("a", 36), "github-token", true},
		{"github fine-grained token", "github_pat_" + strings.Repeat("a", 22), "github-token", true},
		{"slack token", "xoxb-" + "1234567890-abcdef", "slack-token", true},
		{"google api key", "AIza" + strings.Repeat("b", 35), "google-api-key", true},
		{"stripe secret key", "sk_" + "live_" + strings.Repeat("c", 24), "stripe-key", true},
		{"stripe restricted key", "rk_" + "live_" + strings.Repeat("c", 24), "stripe-key", true},
		{"anthropic key, not taken for openai's", "sk-" + "ant-" + "api03-" + strings.Repeat("d", 20), "anthropic-key", true},
		{"openai key", "sk-" + strings.Repeat("e", 48), "openai-key", true},
		{"openai project key", "sk-" + "proj-" + strings.Repeat("e", 20), "openai-key", true},
		{"pgp private key block", "-----BEGIN " + "PGP PRIVATE KEY BLOCK-----", "private-key", true},
		{"gitlab token", "glpat-" + strings.Repeat("f", 20), "gitlab-token", true},
		{"gitlab runner token", "glrt-" + strings.Repeat("f", 20), "gitlab-token", true},
		{"npm token", "npm_" + strings.Repeat("g", 36), "npm-token", true},
		{"slack app token", "xapp-" + "1-A0123456789-1234567890-abcdef", "slack-token", true},
		{"slack webhook", "https://hooks.slack.com/services/" + "T01234567/B01234567/" + strings.Repeat("h", 24), "slack-webhook", true},
		{"postgres url with password", `DATABASE_URL="postgres://app:` + "s3cr3t" + `@db.prod.internal:5432/app"`, "url-password", true},
		{"any scheme with password", "amqp://u:" + "pw" + "@mq.acme.io/vh", "url-password", true},
		{"real url after a local one", "postgres://a:" + "b" + "@localhost/x\nmysql://root:" + "hunter2" + "@10.0.0.5/app", "url-password", true},
		{"jwt", "eyJ" + strings.Repeat("i", 20) + ".eyJ" + strings.Repeat("j", 20) + "." + strings.Repeat("k", 20), "jwt", true},

		{"public pgp block", "-----BEGIN PGP PUBLIC KEY BLOCK-----", "", false},
		{"npm prefix with too few chars", "npm_" + strings.Repeat("g", 35), "", false},
		{"npm_ in an identifier", "my_npm_" + strings.Repeat("g", 36), "", false},
		{"url with user only", "ssh://git@github.com/org/repo.git", "", false},
		{"url password is a variable", "postgres://app:${DB_PASSWORD}@db:5432/app", "", false},
		{"url password is a placeholder", "postgres://user:<password>@host/db", "", false},
		{"url password on localhost", "postgres://postgres:" + "postgres" + "@localhost:5432/test", "", false},
		{"url password on loopback", "redis://:pw" + "@127.0.0.1:6379/0", "", false},
		{"compose url, password is the user", "postgres://postgres:" + "postgres" + "@db:5432/app", "", false},
		{"compose url, dev default password", "mysql://app:" + "Password" + "@mysql:3306/app", "", false},
		{"url password on a reserved example domain", "https://user:" + "pass" + "@api.example.com/v1", "", false},
		{"url without credentials", "https://example.com/a:b@c", "", false},
		{"unsigned jwt", "eyJ" + strings.Repeat("i", 20) + ".eyJ" + strings.Repeat("j", 20) + ".", "", false},
		{"base64 json without a payload", "eyJ" + strings.Repeat("i", 40), "", false},

		{"stripe test key is not live", "sk_test_" + strings.Repeat("c", 24), "", false},
		{"aws prefix with too few chars", "AKIA" + strings.Repeat("Q", 15), "", false},
		{"openai prefix with too few chars", "sk-" + strings.Repeat("e", 19), "", false},
		{"sk- inside a word", "disk-" + strings.Repeat("e", 20), "", false},
		{"public key header", "BEGIN PUBLIC KEY", "", false},
		{"empty", "", "", false},
		{"go source", "package main\n\nfunc main() {}\n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, found := Scan([]byte(tt.content))
			if found != tt.found || rule != tt.want {
				t.Fatalf("Scan() = (%q, %v), want (%q, %v)", rule, found, tt.want, tt.found)
			}
		})
	}
}
