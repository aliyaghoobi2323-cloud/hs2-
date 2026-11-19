//go:build !unix

package main

import "os"

func fileUID(fi os.FileInfo) (uint32, bool) { return 0, false }
