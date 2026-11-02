package cleanup

import "path/filepath"

// SpoolName is the folder, at the mount point of a volume other than the
// layout root's, where a legacy job's relocated tree waits: the relocation it
// came from moved a tree with a rename, which cannot cross volumes. No new job
// gets one.
const SpoolName = ".ccx-cleanup"

// Area is the registered and payload paths of job id under the jobs folder
// jobs.
func Area(jobs, id string) (registered, payload string) {
	dir := filepath.Join(jobs, id)
	return filepath.Join(dir, registeredName), filepath.Join(dir, PayloadName)
}

func (l Layout) holds(jobs string) bool {
	return jobs == l.JobsDir() || filepath.Base(jobs) == "jobs" && filepath.Base(filepath.Dir(jobs)) == SpoolName
}
