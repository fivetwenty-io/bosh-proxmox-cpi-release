package agent

import "sync"

func resetLocalISOStorageWarnOnce() {
	localISOStorageWarnOnce = sync.Once{}
}
