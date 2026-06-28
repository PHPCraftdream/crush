//go:build !dev

//go:generate pnpm install
//go:generate pnpm run build

package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded React build as an fs.FS rooted at "dist/".
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: failed to sub embedded dist: " + err.Error())
	}
	return sub
}
