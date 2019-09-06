package builder

import (
	"context"
	"fmt"
	"log"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
	"github.com/buildbarn/bb-storage/pkg/ac"
	"github.com/buildbarn/bb-storage/pkg/cas"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/longrunning"
	"google.golang.org/genproto/googleapis/rpc/status"
)

type cachingBuildQueue struct {
	actionCache  ac.ActionCache
	contentStore cas.ContentAddressableStorage
}

// NewCachingBuildQueue creates a GRPC service for the Capbilities and
// Execution service that only returns cached actions and doesn't execute anything.
func NewCachingBuildQueue(actionCache ac.ActionCache, contentStore cas.ContentAddressableStorage) BuildQueue {
	return &cachingBuildQueue{
		actionCache:  actionCache,
		contentStore: contentStore,
	}
}

func (bq *cachingBuildQueue) GetCapabilities(ctx context.Context, in *remoteexecution.GetCapabilitiesRequest) (*remoteexecution.ServerCapabilities, error) {
	return &remoteexecution.ServerCapabilities{
		CacheCapabilities: &remoteexecution.CacheCapabilities{
			DigestFunction: []remoteexecution.DigestFunction{
				remoteexecution.DigestFunction_MD5,
				remoteexecution.DigestFunction_SHA1,
				remoteexecution.DigestFunction_SHA256,
			},
			ActionCacheUpdateCapabilities: &remoteexecution.ActionCacheUpdateCapabilities{
				UpdateEnabled: false,
			},
			// CachePriorityCapabilities: Priorities not supported.
			// MaxBatchTotalSize: Not used by Bazel yet.
			SymlinkAbsolutePathStrategy: remoteexecution.CacheCapabilities_ALLOWED,
		},
		ExecutionCapabilities: &remoteexecution.ExecutionCapabilities{
			DigestFunction: remoteexecution.DigestFunction_SHA256,
			ExecEnabled:    true,
		},
		// TODO(edsch): DeprecatedApiVersion.
		LowApiVersion:  &semver.SemVer{Major: 2},
		HighApiVersion: &semver.SemVer{Major: 2},
	}, nil
}

func (bq *cachingBuildQueue) Execute(in *remoteexecution.ExecuteRequest, out remoteexecution.Execution_ExecuteServer) error {
	digest, err := util.NewDigest(in.InstanceName, in.ActionDigest)
	if err != nil {
		return err
	}

	actionResult, err := bq.actionCache.GetActionResult(out.Context(), digest)
	cached := true
	if err != nil {
		actionResult = &remoteexecution.ActionResult{
			ExitCode: 1,
		}
		cached = false
	}

	executeResponse := &remoteexecution.ExecuteResponse{
		Result:       actionResult,
		CachedResult: cached,
		ServerLogs:   map[string]*remoteexecution.LogFile{},
		Message:      "Test not found in cache",
		Status:       &status.Status{Code: 404, Message: "Not found"},
	}

	// Send current state.
	metadata, err := ptypes.MarshalAny(&remoteexecution.ExecuteOperationMetadata{
		Stage:        remoteexecution.ExecuteOperationMetadata_COMPLETED,
		ActionDigest: digest.GetPartialDigest(),
	})
	if err != nil {
		log.Fatal("Failed to marshal execute operation metadata: ", err)
	}
	name := uuid.Must(uuid.NewRandom()).String()
	operation := &longrunning.Operation{
		Name:     name,
		Metadata: metadata,
		Done:     true,
	}
	response, err := ptypes.MarshalAny(executeResponse)
	if err != nil {
		log.Fatal("Failed to marshal execute response: ", err)
	}
	operation.Result = &longrunning.Operation_Response{Response: response}
	if err := out.Send(operation); err != nil {
		return err
	}

	return nil
}

func (bq *cachingBuildQueue) WaitExecution(in *remoteexecution.WaitExecutionRequest, out remoteexecution.Execution_WaitExecutionServer) error {
	return fmt.Errorf("Not implemented")
}
