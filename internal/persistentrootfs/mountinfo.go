package persistentrootfs

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

var mountEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func findMountPoint(input io.Reader, target string, includeChildren bool) (bool, error) {
	want := path.Clean(target)
	prefix := strings.TrimSuffix(want, "/") + "/"
	reader := bufio.NewReader(input)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		if len(line) > 0 {
			fields := strings.SplitN(line, " ", 6)
			if len(fields) < 6 || !strings.HasPrefix(fields[4], "/") {
				return false, fmt.Errorf("invalid mountinfo record: missing mount point")
			}
			point := path.Clean(mountEscapes.Replace(fields[4]))
			if point == want || (includeChildren && strings.HasPrefix(point, prefix)) {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
	}
}
