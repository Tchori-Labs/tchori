//go:build windows

package state

// defaultSyncDir is a documented no-op on Windows.
//
// On POSIX, Save's post-rename durability barrier opens the containing
// directory and calls File.Sync to force the directory entry (the rename)
// to stable storage before Save reports success. That idiom does not
// translate to Windows: File.Sync maps to the Win32 FlushFileBuffers API,
// which requires a write-capable (GENERIC_WRITE) handle, but os.Open on a
// directory only ever yields a read-only handle on Windows. Calling Sync on
// that handle fails with access-denied — and because this barrier runs
// after the atomic rename has already committed, the failure was purely
// cosmetic: state.json was correctly replaced on disk, yet every Save
// returned an error.
//
// Directory fsync is not a supported or necessary durability primitive on
// Windows: NTFS journals metadata operations (including renames) as part of
// its own crash-consistency guarantees, so there is no additional barrier
// for this function to provide. This matches the established precedent in
// the Go ecosystem — google/renameio and boltdb both treat directory fsync
// as a POSIX-only concern and no-op it on Windows — so returning nil here is
// the correct, documented behavior rather than a workaround.
func defaultSyncDir(_ string) error {
	return nil
}
