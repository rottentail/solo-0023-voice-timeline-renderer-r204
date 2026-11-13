package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
)

func (repo *Repository) SaveJob(job domain.RenderJob) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", job.ID+".json")
	if err := writeJSONAtomic(path, job, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render job", err)
	}
	return nil
}

func (repo *Repository) GetJob(id string) (domain.RenderJob, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var job domain.RenderJob
	if err := readJSON(repo.path("jobs", id+".json"), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderJob{}, domain.E(domain.KindNotFound, "render_not_found", "render_id", "render job does not exist")
		}
		return domain.RenderJob{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render job", err)
	}
	return job, nil
}

// StagedRender is an in-progress render publication: a private staging
// directory holding the output stream, committed atomically with its receipt
// or discarded without leaving any visible partial output.
type StagedRender struct {
	repo   *Repository
	id     string
	dir    string
	output *os.File
	closed bool
}

// BeginRenderStaging reserves a staging directory for a render and opens its
// output file. It fails fast if the render identifier is already published.
func (repo *Repository) BeginRenderStaging(id string) (domain.RenderStaging, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	final := repo.path("renders", id)
	if _, err := os.Stat(final); err == nil {
		return nil, domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	temp, err := os.MkdirTemp(repo.path("tmp"), "render-*")
	if err != nil {
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create render staging directory", err)
	}
	output, err := os.OpenFile(filepath.Join(temp, "output.wav"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		_ = os.RemoveAll(temp)
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create render output", err)
	}
	return &StagedRender{repo: repo, id: id, dir: temp, output: output}, nil
}

// OutputWriter returns the staged output stream the renderer writes into.
func (staged *StagedRender) OutputWriter() io.Writer {
	return staged.output
}

// Commit closes and syncs the staged output, writes the receipt beside it,
// and atomically renames the directory into the published renders index. The
// receipt is therefore never visible without the complete output bytes.
func (staged *StagedRender) Commit(receipt domain.RenderReceipt) error {
	staged.repo.mu.Lock()
	defer staged.repo.mu.Unlock()
	if err := staged.closeLocked(); err != nil {
		_ = os.RemoveAll(staged.dir)
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot finalize render output", err)
	}
	final := staged.repo.path("renders", staged.id)
	if _, err := os.Stat(final); err == nil {
		_ = os.RemoveAll(staged.dir)
		return domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(staged.dir)
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	if err := writeJSONAtomic(filepath.Join(staged.dir, "receipt.json"), receipt, 0o640); err != nil {
		_ = os.RemoveAll(staged.dir)
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render receipt", err)
	}
	if err := os.Rename(staged.dir, final); err != nil {
		_ = os.RemoveAll(staged.dir)
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit render artifacts", err)
	}
	if err := syncDirectory(staged.repo.path("renders")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render index", err)
	}
	return nil
}

// Abort discards the staged output, removing every unpublished byte.
func (staged *StagedRender) Abort() error {
	staged.repo.mu.Lock()
	defer staged.repo.mu.Unlock()
	_ = staged.closeLocked()
	return os.RemoveAll(staged.dir)
}

func (staged *StagedRender) closeLocked() error {
	if staged.closed {
		return nil
	}
	staged.closed = true
	if err := staged.output.Sync(); err != nil {
		_ = staged.output.Close()
		return err
	}
	return staged.output.Close()
}

func (repo *Repository) GetRenderOutput(id string) ([]byte, domain.RenderReceipt, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var receipt domain.RenderReceipt
	if err := readJSON(repo.path("renders", id, "receipt.json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render receipt", err)
	}
	payload, err := os.ReadFile(repo.path("renders", id, "output.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render output", err)
	}
	return payload, receipt, nil
}
