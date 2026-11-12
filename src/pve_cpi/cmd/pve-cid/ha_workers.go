package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// workerMode says whether the Director runs its workers, which are the
// processes that run the CPI, under BPM. Only v283.0.0 through v283.1.1 do by
// default, and later versions do only when director.use_bpm_for_workers is
// true. A worker under BPM sees each volume as a mount of its own, while one
// outside BPM sees the host's mounts, which are the ones the doctor sees.
type workerMode int

const (
	// workersUnknown means no Director job on this host says where its
	// workers run, as when the doctor runs somewhere other than a Director.
	workersUnknown workerMode = iota
	// workersOnHost means the workers run outside BPM, in the host's mounts.
	workersOnHost
	// workersUnderBPM means BPM starts each worker in a mount namespace of its
	// own.
	workersUnderBPM
)

// workerMount is what a look inside a running BPM worker found for a parent
// directory and the directory whose mount it must share.
type workerMount int

const (
	// workerSameMount means the worker sees the two on one mount.
	workerSameMount workerMount = iota
	// workerOtherMount means the worker sees them on different mounts.
	workerOtherMount
	// workerNoParent means the parent directory does not exist in the
	// worker, because no volume the worker sees holds it.
	workerNoParent
)

// workerLayout is what the doctor knows about where the CPI runs.
type workerLayout struct {
	mode workerMode
	// inWorker, when set, compares a parent directory with an anchor inside a
	// running BPM worker, as the CPI in that worker sees them.
	inWorker func(parent, anchor string) (workerMount, error)
}

const (
	// directorMonit is the Director job's rendered monit file, which says how
	// each worker starts.
	directorMonit = "/var/vcap/jobs/director/monit"
	// bpmDirectorRun is where BPM keeps the pidfiles of the director job's
	// processes.
	bpmDirectorRun = "/var/vcap/sys/run/bpm/director"
)

var (
	// bpmWorkerStart is how the monit file starts a worker under BPM.
	bpmWorkerStart = regexp.MustCompile(`bpm\s+start\s+director\s+-p\s+(dynamic_disks_)?worker_`)
	// hostWorkerStart is how it starts one outside BPM.
	hostWorkerStart = regexp.MustCompile(`worker_ctl`)
)

// workersFrom reads the Director job's monit file to tell whether its workers
// run under BPM. A missing or unreadable file, or one that starts no worker,
// leaves the answer unknown.
func workersFrom(monit string) workerMode {
	data, err := os.ReadFile(filepath.Clean(monit))
	switch {
	case err != nil:
		return workersUnknown
	case bpmWorkerStart.Match(data):
		return workersUnderBPM
	case hostWorkerStart.Match(data):
		return workersOnHost
	}
	return workersUnknown
}

// systemWorkers is the layout of the host the doctor runs on. Under BPM it
// looks inside a running worker when it can, which needs the doctor to run as
// root on Linux.
func systemWorkers() workerLayout {
	layout := workerLayout{mode: workersFrom(directorMonit)}
	if layout.mode == workersUnderBPM {
		layout.inWorker = bpmWorkerView(bpmDirectorRun)
	}
	return layout
}

// workerLayout returns the layout the collector's cache checks judge with.
func (c *haCollector) workerLayout() workerLayout {
	if c.workers != nil {
		return c.workers()
	}
	return systemWorkers()
}

// bpmVolumes says how a BPM worker sees its volumes.
const bpmVolumes = "each volume a BPM worker sees, such as /var/vcap/store/director or an entry in " +
	"director.cpi_additional_volumes, is a mount of its own even when it sits on the same disk"

// bpmVersions says which Directors run their workers under BPM.
const bpmVersions = "which v283.0.0 through v283.1.1 do by default and later versions do only when " +
	"director.use_bpm_for_workers is true"

// workerSide judges, from the side of the workers that run the CPI, a missing
// directory whose parent this host puts on the same mount as anchor, which
// what names. It returns a non-empty reason when the CPI does not create the
// directory after all. Otherwise it returns the caveat a finding that says the
// CPI creates it adds, which is empty when nothing needs saying. An empty
// anchor means the doctor cannot name the anchor the CPI uses, which is the
// CPI's own temporary directory, so only the caveat is given.
func (j cacheJudge) workerSide(parent, anchor, what string) (reason, caveat string) {
	switch j.workers.mode {
	case workersOnHost:
		return "", ""
	case workersUnderBPM:
		if anchor != "" && j.workers.inWorker != nil {
			found, err := j.workers.inWorker(parent, anchor)
			switch {
			case err != nil:
				// The worker could not be looked into, so the volume rule
				// below stands in for it.
			case found == workerNoParent:
				return fmt.Sprintf("the Director runs its workers under BPM, and inside a running worker its parent "+
					"directory %s does not exist, because no volume the worker sees holds it", parent), ""
			case found == workerOtherMount:
				return fmt.Sprintf("the Director runs its workers under BPM, and inside a running worker its parent "+
					"directory %s is not on the same mount as %s %s, although this host shows one", parent, what,
					anchor), ""
			default:
				return "", ""
			}
		}
		if split := j.bpmVolumeSplit(parent, anchor); split != "" {
			return "the Director runs its workers under BPM, and " + split, ""
		}
		return "", "this check sees the host's mounts, and the Director runs its workers under BPM, where " +
			bpmVolumes + ", so the CPI creates it only when its parent directory sits inside the same volume as " +
			what
	}
	unknown := "this check sees the host's mounts and cannot tell whether the Director runs its workers under " +
		"BPM, " + bpmVersions
	if split := j.bpmVolumeSplit(parent, anchor); split != "" {
		return "", unknown + "; if it does, the CPI does not create it, because " + split +
			", so create it with mode 0700 for the CPI's user"
	}
	return "", unknown + ", and " + bpmVolumes + ", so under BPM the CPI creates it only when its parent " +
		"directory sits inside the same volume as " + what
}

// defaultBPMStore is the directory below which BPM mounts the Director's own
// persistent directory and each store volume as a mount of its own.
const defaultBPMStore = "/var/vcap/store"

// bpmVolumeSplit returns why a CPI under BPM does not create a directory in
// parent although the host's mounts say it would, or an empty string when
// nothing shows that. It stands in for a look inside a running worker. BPM
// mounts the Director's own directory below /var/vcap/store, and the Director
// passes each CPI volume through as given, so with the usual volumes a parent
// outside the directory just below the store that holds the anchor sits on
// another mount inside the worker, even when this host puts both on one disk.
// A volume an operator lists at /var/vcap/store itself or above it would put
// the two on one mount, which only a look inside a worker shows.
func (j cacheJudge) bpmVolumeSplit(parent, anchor string) string {
	if anchor == "" {
		return ""
	}
	store := j.bpmStore
	if store == "" {
		store = defaultBPMStore
	}
	volume := storeVolume(store, anchor)
	if volume == "" || storeVolume(store, parent) == volume {
		return ""
	}
	return fmt.Sprintf("its parent directory %s is outside %s, which holds the allocation journal directory, and "+
		"BPM mounts each volume below %s, such as %s/director or an entry in director.cpi_additional_volumes, "+
		"as a mount of its own, so inside a BPM worker the two are on different mounts although this host shows "+
		"one", parent, volume, store, store)
}

// storeVolume returns the directory just below store that holds path, or an
// empty string when path is store itself or lies outside it.
func storeVolume(store, path string) string {
	rel, err := filepath.Rel(store, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(store, first)
}
