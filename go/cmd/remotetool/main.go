// Main package for the remotetool binary.
//
// This tool supports common debugging operations concerning remotely executed
// actions:
// 1. Download a file or directory from remote cache by its digest.
// 2. Display details of a remotely executed action.
// 3. Download action results by the action digest.
// 4. Re-execute remote action (with optional inputs override).
//
// Example (download an action result from remote action cache):
// bazelisk run //go/cmd/remotetool -- \
//  --operation=download_action_result \
// 	--instance=$INSTANCE \
// 	--service remotebuildexecution.googleapis.com:443 \
// 	--alsologtostderr --v 1 \
// 	--credential_file $CRED_FILE \
// 	--digest=52a54724e6b3dff3bc44ef5dceb3aab5892f2fc7e37fce5aa6e16a7a266fbed6/147 \
// 	--path=`pwd`/tmp
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/filemetadata"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/outerr"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/tool"

	rflags "github.com/bazelbuild/remote-apis-sdks/go/pkg/flags"
	log "github.com/golang/glog"
)

// OpType denotes the type of operation to perform.
type OpType string

const (
	downloadActionResult OpType = "download_action_result"
	showAction           OpType = "show_action"
	downloadBlob         OpType = "download_blob"
	downloadDir          OpType = "download_dir"
	reexecuteAction      OpType = "reexecute_action"
	checkDeterminism     OpType = "check_determinism"
	uploadBlob           OpType = "upload_blob"
	computeTree          OpType = "compute_tree"
)

var supportedOps = []OpType{
	downloadActionResult,
	showAction,
	downloadBlob,
	downloadDir,
	reexecuteAction,
	checkDeterminism,
	uploadBlob,
}

var (
	operation    = flag.String("operation", "", fmt.Sprintf("Specifies the operation to perform. Supported values: %v", supportedOps))
	digest       = flag.String("digest", "", "Digest in <digest/size_bytes> format.")
	pathPrefix   = flag.String("path", "", "Path to which outputs should be downloaded to.")
	inputRoot    = flag.String("input_root", "", "For reexecute_action: if specified, override the action inputs with the specified input root.")
	execAttempts = flag.Int("exec_attempts", 10, "For check_determinism: the number of times to remotely execute the action and check for mismatches.")
	cpuProfFile  = flag.String("pprof_file", "", "File to dump pprof.")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %v [-flags] -- --operation <op> arguments ...\n", path.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	if *operation == "" {
		log.Exitf("--operation must be specified.")
	}
	if *execAttempts <= 0 {
		log.Exitf("--exec_attempts must be >= 1.")
	}

	if *cpuProfFile != "" {
		f, err := os.Create(*cpuProfFile)
		if err != nil {
			log.Fatal("Could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("Could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	ctx := context.Background()
	grpcClient, err := rflags.NewClientFromFlags(ctx)
	if err != nil {
		log.Exitf("error connecting to remote execution client: %v", err)
	}
	defer grpcClient.Close()
	c := &tool.Client{GrpcClient: grpcClient}

	switch OpType(*operation) {
	case downloadActionResult:
		if err := c.DownloadActionResult(ctx, getDigestFlag(), getPathFlag()); err != nil {
			log.Exitf("error downloading action result for digest %v: %v", getDigestFlag(), err)
		}

	case downloadBlob:
		res, err := c.DownloadBlob(ctx, getDigestFlag(), getPathFlag())
		if err != nil {
			log.Exitf("error downloading blob for digest %v: %v", getDigestFlag(), err)
		}
		os.Stdout.Write([]byte(res))

	case downloadDir:
		if err := c.DownloadDirectory(ctx, getDigestFlag(), getPathFlag()); err != nil {
			log.Exitf("error downloading directory for digest %v: %v", getDigestFlag(), err)
		}

	case showAction:
		res, err := c.ShowAction(ctx, getDigestFlag())
		if err != nil {
			log.Exitf("error fetching action %v: %v", getDigestFlag(), err)
		}
		os.Stdout.Write([]byte(res))

	case reexecuteAction:
		if err := c.ReexecuteAction(ctx, getDigestFlag(), *inputRoot, outerr.SystemOutErr); err != nil {
			log.Exitf("error reexecuting action %v: %v", getDigestFlag(), err)
		}

	case checkDeterminism:
		if err := c.CheckDeterminism(ctx, getDigestFlag(), *inputRoot, *execAttempts); err != nil {
			log.Exitf("error checking the determinism of %v: %v", getDigestFlag(), err)
		}

	case uploadBlob:
		if err := c.UploadBlob(ctx, getPathFlag()); err != nil {
			log.Exitf("error uploading blob for digest %v: %v", getDigestFlag(), err)
		}

	case computeTree:
		ComputeTree(grpcClient)

	default:
		log.Exitf("unsupported operation %v. Supported operations:\n%v", *operation, supportedOps)
	}
}

func getDigestFlag() string {
	if *digest == "" {
		log.Exitf("--digest must be specified.")
	}
	return *digest
}

func getPathFlag() string {
	if *pathPrefix == "" {
		log.Exitf("--path must be specified.")
	}
	return *pathPrefix
}

func ComputeTree(grpcClient *client.Client) {
	totalRuns := 0
	beg := time.Now()
	defer func() {
		log.Infof("Ran %v commands in %v time", totalRuns, time.Since(beg).Milliseconds())
	}()

	file, err := os.Open(getPathFlag())
	if err != nil {
		log.Exitf("failed to open input")
	}

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	fmc := filemetadata.NewSingleFlightCache()
	is := command.InputSpec{}
	for scanner.Scan() {
		txt := scanner.Text()

		if strings.TrimSpace(txt) == "" {
			if len(is.Inputs) != 0 || len(is.VirtualInputs) != 0 {
				grpcClient.ComputeMerkleTree(*inputRoot, &is, fmc)
				totalRuns += 1
				is = command.InputSpec{}
			}

			continue
		}

		brokenTxt := strings.Split(txt, ":")
		if len(brokenTxt) != 2 {
			log.Exitf("broken line %v", txt)
		}
		key := strings.TrimSpace(brokenTxt[0])
		v := strings.TrimSpace(brokenTxt[1])

		if key == "inputs" {
			is.Inputs = append(is.Inputs, v)
		} else if key == "path" {
			is.VirtualInputs = append(is.VirtualInputs, &command.VirtualInput{Path: v})
		} else {
			log.Exitf("broken line %v", txt)
		}
	}
}
