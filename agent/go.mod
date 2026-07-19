// This nested module exists solely to exclude the agent skills (and their
// non-buildable example snippets) from the parent module's ./... commands
// (go test, go vet, gofmt). It is not meant to be built or published.
module github.com/shellsecrets/peekastokk/agent

go 1.26
