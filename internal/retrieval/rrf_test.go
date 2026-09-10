package retrieval

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/store"
)

func hit(id byte) store.SearchHit {
	var u uuid.UUID
	u[15] = id
	return store.SearchHit{ChunkID: u, SourceID: string('a' + rune(id))}
}

func TestFuseBothListsOutranksEither(t *testing.T) {
	a, b, c := hit(1), hit(2), hit(3)
	// a is rank 2 in both lists; b is rank 1 in vector only; c rank 1 lexical only.
	out := fuse([]store.SearchHit{b, a}, []store.SearchHit{c, a}, 60)
	require.Len(t, out, 3)
	require.Equal(t, a.ChunkID, out[0].hit.ChunkID, "a chunk in both lists must outrank one in either alone at equal rank")
	require.Equal(t, 2, out[0].vectorRank)
	require.Equal(t, 2, out[0].lexicalRank)
	// b and c tie on RRF; b wins on vector rank.
	require.Equal(t, b.ChunkID, out[1].hit.ChunkID)
	require.Equal(t, c.ChunkID, out[2].hit.ChunkID)
	require.Zero(t, out[2].vectorRank, "absent from vector list")
}

func TestFuseKBehaviour(t *testing.T) {
	a, b := hit(1), hit(2)
	// Small k exaggerates rank differences; a at rank 1 in one list must beat
	// b at rank 2 in the same list regardless of k.
	for _, k := range []int{1, 60, 1000} {
		out := fuse([]store.SearchHit{a, b}, nil, k)
		require.Equal(t, a.ChunkID, out[0].hit.ChunkID, "k=%d", k)
		require.Greater(t, out[0].rrf, out[1].rrf)
	}
	// k<=0 falls back to the default rather than dividing by rank alone.
	out := fuse([]store.SearchHit{a}, nil, 0)
	require.InDelta(t, 1.0/61.0, out[0].rrf, 1e-9)
}

func TestFuseDeterministicTies(t *testing.T) {
	a, b := hit(1), hit(2)
	// Same ranks in mirrored lists: pure tie, broken by chunk id.
	first := fuse([]store.SearchHit{a}, []store.SearchHit{b}, 60)
	for i := 0; i < 10; i++ {
		again := fuse([]store.SearchHit{a}, []store.SearchHit{b}, 60)
		require.Equal(t, first[0].hit.ChunkID, again[0].hit.ChunkID)
		require.Equal(t, first[1].hit.ChunkID, again[1].hit.ChunkID)
	}
}

func TestFuseEmptyLists(t *testing.T) {
	require.Empty(t, fuse(nil, nil, 60))
	out := fuse(nil, []store.SearchHit{hit(1)}, 60)
	require.Len(t, out, 1)
	require.Equal(t, 1, out[0].lexicalRank)
}
