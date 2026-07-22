package dataset

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// DictionarySize is the trained dictionary's target size. 112 KB is large
// enough to hold the shared vocabulary of these payloads (the JSON keys, the
// handful of release statuses, the country and type names) and small enough
// that shipping it once is negligible against a multi-gigabyte artifact.
const DictionarySize = 112 * 1024

// dictionaryMinSamples is the fewest payloads worth training on. Below this a
// dictionary is noise, so the build ships without one and every payload still
// decompresses, just less tightly.
const dictionaryMinSamples = 512

// trainDictionary builds a zstd dictionary from sample payloads.
//
// The training itself is done by the zstd binary, because the Go zstd library
// can use a dictionary but not produce one. It is a build-machine dependency,
// never a runtime one, and its absence is not fatal: a build without it
// simply compresses without a dictionary. The samples are the raw, pre
// compression payloads, which is what the trainer needs.
func trainDictionary(samples [][]byte) ([]byte, error) {
	if len(samples) < dictionaryMinSamples {
		return nil, nil
	}
	bin, err := exec.LookPath("zstd")
	if err != nil {
		return nil, nil
	}

	dir, err := os.MkdirTemp("", "lmp-dict-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// The trainer reads one file per sample. Writing them is cheap next to
	// the training pass and the build as a whole.
	names := make([]string, 0, len(samples))
	for i, s := range samples {
		name := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(name, s, 0o644); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	out := filepath.Join(dir, "dict")
	args := append([]string{"--train", "--maxdict=" + strconv.Itoa(DictionarySize), "-o", out}, names...)
	cmd := exec.Command(bin, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("dataset: training dictionary: %w: %s", err, combined)
	}

	dict, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	return dict, nil
}
