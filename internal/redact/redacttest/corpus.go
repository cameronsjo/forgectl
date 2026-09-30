// Package redacttest holds the argv corpus that #749's review ran against
// redact.Args: user-written command lines that carry a credential in a shape
// no URL rule sees. Tests at every site that passes user argv to a Runner
// (workflow run steps, docker build/run/shell) run it through the real Runner
// and assert Secret reaches no rendering.
package redacttest

// Secret is the credential every Corpus row carries.
const Secret = "SEKRIT" //nolint:gosec // G101: a fake credential the corpus plants

// Corpus is the #749 review's 92 argv rows, the tool name first. The
// look-alike flag rows spell their zero-width space, em dash and fullwidth
// hyphens as escapes.
var Corpus = [][]string{
	// curl basic auth / user
	{"curl", "-u", "user:SEKRIT", "https://h/x"},
	{"curl", "-uuser:SEKRIT", "https://h/x"},
	{"curl", "--user", "user:SEKRIT"},
	{"curl", "--oauth2-bearer", "SEKRIT"},
	{"curl", "-d", `{"access_token":"SEKRIT"}`},
	{"curl", "--data-raw", `{"password":"SEKRIT"}`},
	// mysql/psql style glued short flag
	{"docker", "run", "img", "mysql", "-pSEKRIT"},
	{"docker", "run", "img", "mysqladmin", "-p", "SEKRIT"},
	// gh api raw field / JSON
	{"gh", "api", "-f", "token=SEKRIT"},
	{"gh", "api", "--raw-field", `body={"token":"SEKRIT"}`},
	{"gh", "secret", "set", "FOO", "--body", "SEKRIT"},
	{"gh", "secret", "set", "FOO", "-b", "SEKRIT"},
	{"gh", "secret", "set", "FOO", "-bSEKRIT"},
	{"gh", "variable", "set", "API_TOKEN", "--body", "SEKRIT"},
	// docker
	{"docker", "build", "--build-arg", "NPM_TOKEN=SEKRIT", "."},
	{"docker", "build", "--build-arg=NPM_TOKEN=SEKRIT", "."},
	{"docker", "run", "-e", "AWS_SECRET_ACCESS_KEY=SEKRIT", "img"},
	{"docker", "run", "--env=DB_PASS=SEKRIT", "img"},
	{"docker", "run", "-eDB_PASSWORD=SEKRIT", "img"},
	{"docker", "login", "-u", "me", "-p", "SEKRIT"},
	{"docker", "login", "-pSEKRIT"},
	{"docker", "login", "--password", "SEKRIT"},
	{"docker", "run", "-e", "OPENAI_APIKEY=SEKRIT"},
	{"docker", "run", "-e", "ANTHROPIC_API_KEY=SEKRIT"},
	{"docker", "run", "-e", "DATABASE_URL=postgres://u:SEKRIT@h/db"},
	{"docker", "run", "-e", "DSN=u:SEKRIT@tcp(h)/db"},
	{"docker", "run", "-e", "REDIS=redis:SEKRIT"},
	// env var names without fragments
	{"env", "GH_ENTERPRISE_TOKEN=SEKRIT"},
	{"env", "OP_SESSION_my=SEKRIT"},
	{"env", "AWS_SESSION=SEKRIT"},
	{"env", "HF_HUB=SEKRIT"},
	{"env", "CLOUDFLARE_API_KEY=SEKRIT"},
	{"env", "SLACK_WEBHOOK=https-less/SEKRIT"},
	{"env", "PRIVATE_KEY=SEKRIT"},
	{"env", "JWT=SEKRIT"},
	{"env", "CREDS=SEKRIT"},
	{"env", "APIKEY=SEKRIT"},
	{"env", "KEYS=SEKRIT"},
	// ssh / sshpass
	{"sshpass", "-pSEKRIT", "ssh", "h"},
	{"sshpass", "-p", "SEKRIT", "ssh", "h"},
	// kubectl
	{"kubectl", "--token=SEKRIT", "get", "pods"},
	{"kubectl", "create", "secret", "generic", "x", "--from-literal=password=SEKRIT"},
	{"kubectl", "create", "secret", "generic", "x", "--from-literal=db=SEKRIT"},
	{"kubectl", "config", "set-credentials", "me", "--token=SEKRIT"},
	// npm
	{"npm", "config", "set", "//registry.npmjs.org/:_authToken", "SEKRIT"},
	{"npm", "config", "set", "//registry.npmjs.org/:_authToken=SEKRIT"},
	{"npm", "--//registry.npmjs.org/:_authToken=SEKRIT", "install"},
	// git
	{"git", "-c", "credential.helper=!echo password=SEKRIT", "fetch"},
	{"git", "-c", "http.extraHeader", "Authorization: Bearer SEKRIT"},
	{"git", "-c", "core.sshCommand=sshpass -pSEKRIT ssh", "fetch"},
	{"git", "clone", "https://x-access-token:SEKRIT@github.com/o/r"},
	{"git", "remote", "add", "o", "https://SEKRIT@github.com/o/r"},
	// header shapes
	{"curl", "-H", "X-Auth: SEKRIT"},
	{"curl", "-H", "Authorization:Bearer SEKRIT"},
	{"curl", "-HAuthorization: SEKRIT"},
	{"curl", "--header", "Cookie:SEKRIT"},
	{"curl", "-b", "session=SEKRIT"},
	{"curl", "--cookie", "session=SEKRIT"},
	{"tool", "Bearer", "SEKRIT"},
	{"tool", "token SEKRIT"},
	{"tool", "Token", "SEKRIT"},
	// query strings
	{"tool", "https-less?token=SEKRIT"},
	{"tool", "x?key=SEKRIT"},
	{"tool", "x?code=SEKRIT"},
	{"tool", "x?apikey=SEKRIT"},
	{"tool", "x?client_secret=SEKRIT"},
	{"tool", "x?X-Goog-Credential=a&X-Goog-Signature=SEKRIT"},
	// misc
	{"op", "signin", "--raw"},
	{"security", "add-generic-password", "-s", "svc", "-a", "me", "-w", "SEKRIT"},
	{"security", "add-generic-password", "-wSEKRIT"},
	{"tool", "--pass=SEKRIT"},
	{"tool", "--passwd", "SEKRIT"},
	{"tool", "-P", "SEKRIT"},
	{"tool", "--key", "SEKRIT"},
	{"tool", "--secret-key=SEKRIT"},
	{"tool", "--otp", "SEKRIT"},
	{"tool", "--pin", "SEKRIT"},
	{"tool", "--session", "SEKRIT"},
	{"tool", "--sas", "SEKRIT"},
	{"tool", "--creds", "SEKRIT"},
	{"tool", "--jwt", "SEKRIT"},
	{"tool", "--webhook", "SEKRIT"},
	{"tool", "--private-key=SEKRIT"},
	{"tool", "--token", "--", "SEKRIT"},
	{"tool", "--token", "[redacted]", "SEKRIT"},
	{"tool", "--TOKEN\u200b", "SEKRIT"},
	{"tool", "\u2014token", "SEKRIT"},
	{"tool", "\uff0d\uff0dtoken", "SEKRIT"},
	{"tool", "--to\u200bken=SEKRIT"},
	{"tool", " --token", "SEKRIT"},
	{"tool", "-H", "-H", "SEKRIT"},
	{"tool", "--token", "--password", "SEKRIT"},
}
