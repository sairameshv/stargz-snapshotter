/*
   Copyright The containerd Authors.

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

package ipfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	"github.com/containerd/stargz-snapshotter/ipfs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ResolveHandler struct{}

func (r *ResolveHandler) Handle(ctx context.Context, desc ocispec.Descriptor) (remote.Fetcher, int64, error) {
	cid, err := ipfs.GetCID(desc)
	if err != nil {
		return nil, 0, err
	}
	sizeB, err := exec.Command("ipfs", "files", "stat", "--format=<size>", "/ipfs/"+cid).Output()
	if err != nil {
		return nil, 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSuffix(string(sizeB), "\n"), 10, 64)
	if err != nil {
		return nil, 0, err
	}
	return &fetcher{cid: cid, size: size}, size, nil
}

type fetcher struct {
	cid  string
	size int64
}

func (f *fetcher) Fetch(ctx context.Context, off int64, size int64) (io.ReadCloser, error) {
	if off > f.size {
		return nil, fmt.Errorf("offset is larger than the size of the blob %d(offset) > %d(blob size)", off, f.size)
	}
	pr, pw := io.Pipe()
	go func() {
		maxretry := 100
		curoff := off
		for i := 0; ; i++ {
			cont, err := func() (cont bool, err error) { // defer scope
				cmd := exec.Command("ipfs", "cat", fmt.Sprintf("--offset=%d", curoff), fmt.Sprintf("--length=%d", size), f.cid)
				stderrbuf := new(bytes.Buffer)
				cmd.Stderr = stderrbuf
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					return false, err
				}
				if err := cmd.Start(); err != nil {
					return false, err
				}
				defer func() {
					go func() {
						// fully read until EOF
						io.Copy(io.Discard, stdout)
						cmd.Wait()
					}()
				}()
				if n, err := io.CopyN(pw, stdout, size); err != nil {
					sb, _ := io.ReadAll(stderrbuf)
					if i < maxretry && strings.Contains(string(sb), "someone else has the lock") {
						log.G(ctx).WithError(err).WithField("stderr", string(sb)).Debugf("retrying copy %q(offset:%d,length:%d,actuallength:%d,retry:%d/%d)", f.cid, off, size, n, i, maxretry)
						// we need to retry until we can get the lock
						time.Sleep(time.Second)
						curoff += n
						return true, nil
					}
					log.G(ctx).WithError(err).WithField("stderr", string(sb)).Debugf("failed to copy %q(offset:%d,length:%d,actuallength:%d,retry:%d/%d)", f.cid, off, size, n, i, maxretry)
					return false, err
				}
				return false, nil
			}()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if cont {
				continue
			}
			break
		}
		pw.Close()
	}()
	return pr, nil
}

func (f *fetcher) Check() error {
	return exec.Command("ipfs", "files", "stat", "/ipfs/"+f.cid).Run()
}

func (f *fetcher) GenID(off int64, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", f.cid, off, size)))
	return fmt.Sprintf("%x", sum)
}

type readCloser struct {
	io.Reader
	closeFunc func() error
}

func (r *readCloser) Close() error { return r.closeFunc() }
