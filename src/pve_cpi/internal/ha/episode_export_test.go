package ha

// SetEpisodeBeforeWrite sets the hook that runs under the record's lock just
// before the record is written, so a test can make another observation meet
// this one in the middle of its change.
func SetEpisodeBeforeWrite(s *EpisodeStore, hook func()) { s.beforeWrite = hook }
