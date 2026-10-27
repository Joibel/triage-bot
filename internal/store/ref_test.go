package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRef(t *testing.T) {
	t.Parallel()

	const fork = "argoproj/argo-workflows-ghsa-abcd-efgh-ijkl"
	tests := []struct {
		in      string
		want    Ref
		wantErr string
	}{
		{in: "#123", want: Ref{Repo: mainRepo, Number: 123}},
		{in: "123", want: Ref{Repo: mainRepo, Number: 123}},
		{in: " 123 ", want: Ref{Repo: mainRepo, Number: 123}},
		{in: "argoproj/argo-workflows#7", want: Ref{Repo: mainRepo, Number: 7}},
		{in: "https://github.com/argoproj/argo-workflows/issues/8123", want: Ref{Repo: mainRepo, Number: 8123}},
		{in: "https://github.com/argoproj/argo-workflows/pull/9500/files#diff-1", want: Ref{Repo: mainRepo, Number: 9500}},
		{in: "https://github.com/Argoproj/Argo-Workflows/pull/1", want: Ref{Repo: mainRepo, Number: 1}},
		{in: "GHSA-ABCD-efgh-1234", want: Ref{Repo: mainRepo, GHSA: "GHSA-abcd-efgh-1234"}},
		{in: "ghsa-abcd-efgh-1234", want: Ref{Repo: mainRepo, GHSA: "GHSA-abcd-efgh-1234"}},
		{in: "https://github.com/argoproj/argo-workflows/security/advisories/GHSA-abcd-efgh-1234",
			want: Ref{Repo: mainRepo, GHSA: "GHSA-abcd-efgh-1234"}},
		{in: "argo-workflows-ghsa-abcd-efgh-ijkl#2", want: Ref{Repo: fork, Number: 2}},
		{in: fork + "#2", want: Ref{Repo: fork, Number: 2}},
		{in: "https://github.com/" + fork + "/pull/3", want: Ref{Repo: fork, Number: 3}},

		{in: "", wantErr: "empty"},
		{in: "#0", wantErr: "not a reference"},
		{in: "#abc", wantErr: "not a reference"},
		{in: "kubernetes/kubernetes#1", wantErr: "not argoproj/argo-workflows"},
		{in: "argoproj/argo-cd#1", wantErr: "not argoproj/argo-workflows"},
		{in: "argo-workflows-ghsa-nope#1", wantErr: "not argoproj/argo-workflows"},
		{in: "https://gitlab.com/argoproj/argo-workflows/issues/1", wantErr: "not a github.com URL"},
		{in: "https://github.com/argoproj/argo-workflows/blob/main/README.md", wantErr: "does not point at"},
		{in: "https://github.com/" + fork + "/security/advisories/GHSA-abcd-efgh-1234", wantErr: "advisories belong to"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRef(tc.in, mainRepo)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Display must render something ParseRef reads back to the same ref, so what
// the CLI prints can be pasted into the next command.
func TestRefDisplayRoundTrips(t *testing.T) {
	t.Parallel()

	for _, r := range []Ref{
		{Repo: mainRepo, Number: 8123},
		{Repo: mainRepo, GHSA: "GHSA-abcd-efgh-1234"},
		{Repo: "argoproj/argo-workflows-ghsa-abcd-efgh-ijkl", Number: 2},
	} {
		got, err := ParseRef(r.Display(mainRepo), mainRepo)
		require.NoError(t, err)
		assert.Equal(t, r, got)
	}
}
