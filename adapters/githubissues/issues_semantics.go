package githubissues

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mateusememe/syntroph/storage"
)

// issueOperations is the transport boundary beneath the shared Issues mirror
// semantics. REST and MCP differ only in how these operations are executed.
type issueOperations interface {
	ensureLabels(context.Context) error
	createIssue(context.Context, storage.SessionDiary) (issue, error)
	closeIssue(context.Context, int) (issue, error)
	getIssue(context.Context, string) (issue, error)
	effectiveRemote(context.Context, storage.RemoteBinding) (effectiveRemote, error)
	findCorrection(context.Context, int, storage.SessionDiary, string) (comment, error)
	createCorrection(context.Context, int, storage.SessionDiary, string) (comment, error)
	findMarked(context.Context, string) (issue, error)
	failed(storage.MirrorResult, error) storage.MirrorResult
}

type issueSemantics struct {
	provider string
	bindings *storage.BindingStore
	ops      issueOperations
}

var _ issueOperations = (*Provider)(nil)
var _ issueOperations = mcpIssueOperations{}

func (s issueSemantics) base(d storage.SessionDiary) storage.MirrorResult {
	return storage.MirrorResult{Backend: storage.BackendIssues, Provider: s.provider, Key: d.Key(), LocalHash: hash(d.Content)}
}

func (s issueSemantics) mirror(ctx context.Context, diary storage.SessionDiary, recovery bool) storage.MirrorResult {
	r := s.base(diary)
	if err := diary.Validate(); err != nil {
		return pending(r, err)
	}
	binding, bound, err := s.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	var remote issue
	if bound {
		if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
			return s.compareEffective(ctx, r, diary.Content, binding)
		}
		remote, err = s.ops.getIssue(ctx, binding.RemoteID)
	} else if recovery {
		remote, err = s.ops.findMarked(ctx, r.Key)
		if errors.Is(err, errNotFound) {
			err = nil
		}
	}
	if err != nil {
		return s.ops.failed(r, err)
	}
	if remote.Number != 0 {
		return s.finishExisting(ctx, r, diary, remote)
	}
	if err := s.ops.ensureLabels(ctx); err != nil {
		return s.ops.failed(r, err)
	}
	remote, err = s.ops.createIssue(ctx, diary)
	if err != nil {
		return s.ops.failed(r, err)
	}
	created := remote
	provisional := observedIssue(r, created, diary.Content)
	bindingErr := s.bindings.SaveResult(ctx, s.provider, provisional)
	remote, err = s.ops.closeIssue(ctx, remote.Number)
	if err != nil {
		if bindingErr != nil {
			err = errors.Join(err, fmt.Errorf("persist provisional remote binding: %w", bindingErr))
		}
		return observedIssue(s.ops.failed(r, err), created, diary.Content)
	}
	result := mirroredIssue(r, remote, diary.Content)
	if bindingErr != nil {
		return pending(result, fmt.Errorf("persist provisional remote binding: %w", bindingErr))
	}
	return result
}

func (s issueSemantics) status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	r := s.base(diary)
	binding, bound, err := s.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	if !bound {
		return pending(r, errors.New("remote binding not found; run explicit storage recovery"))
	}
	if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
		return s.compareEffective(ctx, r, diary.Content, binding)
	}
	remote, err := s.ops.getIssue(ctx, binding.RemoteID)
	if err != nil {
		return s.ops.failed(r, err)
	}
	return compareIssue(r, diary.Content, remote)
}

func (s issueSemantics) resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observed string) storage.MirrorResult {
	r := s.base(diary)
	if choice != storage.KeepLocal && choice != storage.KeepRemote {
		return issueConflict(r, issue{}, errors.New("resolution must be keep-local or keep-remote"))
	}
	binding, bound, err := s.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	if !bound {
		return pending(r, errors.New("remote binding not found"))
	}
	effective, err := s.ops.effectiveRemote(ctx, binding)
	if err != nil {
		return s.ops.failed(r, err)
	}
	r.RemoteID, r.RemoteURL, r.RemoteRev, r.ExpectedRev = binding.RemoteID, binding.URL, effective.revision, observed
	r.RemoteContent, r.EffectiveRemoteHash = effective.content, hash(effective.content)
	if observed == "" || observed != effective.revision {
		r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict
		return r
	}
	if choice == storage.KeepRemote {
		r.State = storage.Mirrored
		return r
	}
	if effective.exact && effective.content == diary.Content {
		r.State, r.RemoteContent = storage.Mirrored, ""
		return r
	}
	number, err := strconv.Atoi(binding.RemoteID)
	if err != nil || number <= 0 {
		return pending(r, errors.New("remote binding contains an invalid Issue number"))
	}
	correction, err := s.ops.findCorrection(ctx, number, diary, effective.revision)
	if errors.Is(err, errNotFound) {
		correction, err = s.ops.createCorrection(ctx, number, diary, effective.revision)
	}
	if err != nil {
		return s.ops.failed(r, err)
	}
	r.State, r.RemoteRev, r.EffectiveRemoteHash = storage.Mirrored, commentRevision(correction), hash(diary.Content)
	r.RemoteID, r.RemoteURL = binding.RemoteID, binding.URL
	return r
}

func (s issueSemantics) finishExisting(ctx context.Context, r storage.MirrorResult, diary storage.SessionDiary, remote issue) storage.MirrorResult {
	compared := compareIssue(r, diary.Content, remote)
	if compared.State != storage.Mirrored {
		return compared
	}
	if remote.State != "closed" || remote.StateReason != "completed" {
		closed, err := s.ops.closeIssue(ctx, remote.Number)
		if err != nil {
			return observedIssue(s.ops.failed(r, err), remote, diary.Content)
		}
		return mirroredIssue(r, closed, diary.Content)
	}
	return compared
}

func observedIssue(r storage.MirrorResult, remote issue, content string) storage.MirrorResult {
	r.RemoteID, r.RemoteURL, r.RemoteRev = strconv.Itoa(remote.Number), remote.HTMLURL, issueRevision(remote)
	r.EffectiveRemoteHash = hash(content)
	return r
}

func (s issueSemantics) compareEffective(ctx context.Context, r storage.MirrorResult, local string, binding storage.RemoteBinding) storage.MirrorResult {
	effective, err := s.ops.effectiveRemote(ctx, binding)
	if err != nil {
		return s.ops.failed(r, err)
	}
	r.RemoteID, r.RemoteURL, r.RemoteRev = binding.RemoteID, binding.URL, effective.revision
	r.RemoteContent, r.EffectiveRemoteHash = effective.content, hash(effective.content)
	if effective.exact && effective.content == local {
		r.State, r.RemoteContent = storage.Mirrored, ""
		return r
	}
	r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict
	return r
}

func compareIssue(r storage.MirrorResult, local string, remote issue) storage.MirrorResult {
	remoteContent, exact := stripDiaryMarker(remote.Body, r.Key)
	r.RemoteID, r.RemoteURL, r.RemoteRev = strconv.Itoa(remote.Number), remote.HTMLURL, issueRevision(remote)
	r.RemoteContent, r.EffectiveRemoteHash = remote.Body, hash(remote.Body)
	if exact {
		r.RemoteContent, r.EffectiveRemoteHash = remoteContent, hash(remoteContent)
	}
	if exact && remoteContent == local {
		r.State, r.EffectiveRemoteHash, r.RemoteContent = storage.Mirrored, hash(local), ""
		return r
	}
	return issueConflict(r, remote, storage.ErrConflict)
}

func issueConflict(r storage.MirrorResult, remote issue, err error) storage.MirrorResult {
	r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, err
	if remote.Number != 0 {
		r.RemoteID, r.RemoteURL, r.RemoteRev, r.RemoteContent, r.EffectiveRemoteHash = strconv.Itoa(remote.Number), remote.HTMLURL, issueRevision(remote), remote.Body, hash(remote.Body)
		if content, exact := stripDiaryMarker(remote.Body, r.Key); exact {
			r.RemoteContent, r.EffectiveRemoteHash = content, hash(content)
		}
	}
	return r
}
