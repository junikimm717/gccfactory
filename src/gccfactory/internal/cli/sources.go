package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/sources"
)

var cmdSources = &command{
	Name:     "sources",
	Short:    "list the pinned upstream tarballs and their checksums",
	Synopsis: "gccfactory sources [--urls] [--json]",
	Long: `Every input to the build is pinned by sha256 in
src/gccfactory/internal/sources/sources.json, which is compiled into the binary.
A tarball is only used once its sha256 matches, and the ` + "`cached`" + ` column tells
you whether it is already downloaded and verified in dist/src/.

Because the checksums are part of every job's content key, editing this file
invalidates exactly the artifacts that depend on the changed source.

To bump a version, edit sources.json and re-run src/gccfactory/update-sources.sh,
which re-downloads each URL and rewrites the checksums.

FLAGS
  --urls   show the download mirrors for each source
  --json   emit the raw pinned data`,
	Run: runSources,
}

func runSources(g *Global, args []string) error {
	fs := g.flagSet("sources")
	urls := fs.Bool("urls", false, "show download mirrors")
	asJSON := fs.Bool("json", false, "emit raw pinned data as JSON")
	if err := parse(fs, args); err != nil {
		return finish("sources", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}
	all := sources.All()

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}

	t := newTable("NAME", "VERSION", "SHA256", "CACHED")
	for _, s := range all {
		t.add(s.Name, s.Version, dim(s.SHA256), cachedMark(g.Dist, s))
	}
	t.render(os.Stdout)
	if *urls {
		fmt.Println()
		for _, s := range all {
			fmt.Printf("%s\n", bold(s.Name+" "+s.Version))
			for _, u := range s.URLs {
				fmt.Printf("  %s\n", u)
			}
		}
	}
	fmt.Printf("\n%s\n", dim("cache: "+filepath.Join(g.Dist, "src")))
	return nil
}

// cachedMark reports whether the tarball is present and verified, matching the
// dist/src/<sha[:16]>-<filename>.done stamp convention.
func cachedMark(dist string, s sources.Source) string {
	if len(s.SHA256) < 16 {
		return dim("?")
	}
	matches, _ := filepath.Glob(filepath.Join(dist, "src", s.SHA256[:16]+"-*.done"))
	if len(matches) > 0 {
		return green("yes")
	}
	return dim("no")
}
