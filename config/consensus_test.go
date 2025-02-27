// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"testing"

	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/stretchr/testify/require"
)

func TestConsensusParams(t *testing.T) {
	partitiontest.PartitionTest(t)

	for proto, params := range Consensus {
		// Our implementation of Payset.Commit() assumes that
		// SupportSignedTxnInBlock implies not PaysetCommitUnsupported.
		if params.SupportSignedTxnInBlock && params.PaysetCommit == PaysetCommitUnsupported {
			t.Errorf("Protocol %s: SupportSignedTxnInBlock with PaysetCommitUnsupported", proto)
		}

		// ApplyData requires not PaysetCommitUnsupported.
		if params.ApplyData && params.PaysetCommit == PaysetCommitUnsupported {
			t.Errorf("Protocol %s: ApplyData with PaysetCommitUnsupported", proto)
		}
	}
}

// TestConsensusUpgradeWindow ensures that the upgrade window is a non-zero value, and confirm to be within the valid range.
func TestConsensusUpgradeWindow(t *testing.T) {
	partitiontest.PartitionTest(t)

	for proto, params := range Consensus {
		require.GreaterOrEqualf(t, params.MaxUpgradeWaitRounds, params.MinUpgradeWaitRounds, "Version :%v", proto)
		for toVersion, delay := range params.ApprovedUpgrades {
			if params.MinUpgradeWaitRounds != 0 || params.MaxUpgradeWaitRounds != 0 {
				require.NotZerof(t, delay, "From :%v\nTo :%v", proto, toVersion)
				require.GreaterOrEqualf(t, delay, params.MinUpgradeWaitRounds, "From :%v\nTo :%v", proto, toVersion)
				require.LessOrEqualf(t, delay, params.MaxUpgradeWaitRounds, "From :%v\nTo :%v", proto, toVersion)
			} else {
				require.Zerof(t, delay, "From :%v\nTo :%v", proto, toVersion)

			}
		}
	}
}
