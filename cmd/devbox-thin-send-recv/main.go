/*
Copyright 2025 The containerd Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/containerd/containerd/snapshots/devbox/lvm"
)

func main() {
	var (
		baseLV   = flag.String("base", "", "base thin LV path (e.g. /dev/vg/devbox-base)")
		targetLV = flag.String("target", "", "target thin LV path (e.g. /dev/vg/devbox-snapshot)")
		outFile  = flag.String("out", "", "output file path for thin_send stream")
	)
	flag.Parse()

	if *baseLV == "" || *targetLV == "" || *outFile == "" {
		fmt.Fprintln(os.Stderr, "missing required flags: -base, -target, -out")
		flag.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	if err := lvm.ThinSendToFile(ctx, *baseLV, *targetLV, *outFile); err != nil {
		fmt.Fprintf(os.Stderr, "thin_send failed: %v\n", err)
		os.Exit(1)
	}
}
