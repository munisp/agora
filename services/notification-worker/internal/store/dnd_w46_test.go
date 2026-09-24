package store

// SPEC-W46 PERF-19/22 regression tests: ImportGlobalDND batched insert —
// duplicates WITHIN one import call must not trip the ON CONFLICT
// cardinality violation, and imports larger than dndImportBatchSize must
// chunk while keeping the count-of-new-rows contract.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDNDImportBatchDedupeAndChunking(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()

	// Within-batch duplicates (incl. post-normalization) count once.
	inserted, err := st.ImportGlobalDND(ctx,
		[]string{"+2348077777777", "+234 807 777 7777", "+2348077777777"}, "ncc2442")
	require.NoError(t, err)
	require.Equal(t, 1, inserted, "intra-batch duplicates insert one row")

	// Larger than one batch: exercises the chunk loop; all rows new.
	bulk := make([]string, 0, dndImportBatchSize+7)
	for i := 0; i < dndImportBatchSize+7; i++ {
		bulk = append(bulk, fmt.Sprintf("+23490%08d", i))
	}
	inserted, err = st.ImportGlobalDND(ctx, bulk, "ncc2442")
	require.NoError(t, err)
	require.Equal(t, dndImportBatchSize+7, inserted)

	// Re-importing the same snapshot (plus one new) is idempotent across
	// batch boundaries and returns only the NEW count.
	bulk = append(bulk, "+2349099999999")
	inserted, err = st.ImportGlobalDND(ctx, bulk, "ncc2442")
	require.NoError(t, err)
	require.Equal(t, 1, inserted)
}
