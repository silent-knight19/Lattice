package memtable_test

import (
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func FuzzSkipList_ConcurrentReaderOperations(f *testing.F) {
	// Seed corpus with various sequences of actions
	f.Add([]byte{0x01, 0x02, 'k', '1', 0x03, 'v', 'a', 'l'})
	f.Add([]byte{0x02, 0x01, 'a', 0x01, 0x01, 'b', 0x02, 0x01, 'b'})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 {
			return
		}

		sl := memtable.NewSkipList()
		stopReaders := make(chan struct{})
		var wg sync.WaitGroup

		// Fixed small number of background readers (2 goroutines) to avoid goroutine bombs
		for r := 0; r < 2; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stopReaders:
						return
					default:
						// Search random byte prefixes
						_, _ = sl.SearchConcurrent([]byte("fuzz-key"))
					}
				}
			}()
		}

		cursor := 0
		seq := uint64(1)
		opCount := 0
		const maxOps = 40 // Strictly bound operations per fuzz iteration

		for cursor < len(data) && opCount < maxOps {
			opCount++
			action := data[cursor] % 2 // 0: PUT, 1: DELETE
			cursor++

			if cursor >= len(data) {
				break
			}

			keyLen := int(data[cursor] % 16) // Small bounded key length [0, 15]
			cursor++
			if cursor+keyLen > len(data) {
				break
			}
			keyBytes := data[cursor : cursor+keyLen]
			cursor += keyLen

			switch action {
			case 0: // PUT
				valLen := 0
				var valBytes []byte
				if cursor < len(data) {
					valLen = int(data[cursor] % 32)
					cursor++
					if cursor+valLen <= len(data) {
						valBytes = data[cursor : cursor+valLen]
						cursor += valLen
					}
				}

				key, keyErr := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypePut)
				seq++
				if keyErr == nil {
					_ = sl.Insert(key, valBytes)
				}

			case 1: // DELETE
				key, keyErr := binary.NewInternalKey(keyBytes, binary.SeqNum(seq), binary.OpTypeDelete)
				seq++
				if keyErr == nil {
					_ = sl.Insert(key, nil)
				}
			}
		}

		close(stopReaders)
		wg.Wait()

		// Validate structural invariants after concurrent fuzzing
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("structural invariant violated after concurrent fuzzing: %v", err)
		}
	})
}
