package agent

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"codenite/worker/internal/config"
	"codenite/worker/internal/util"
)

type Worker struct {
	cfg        config.Config
	taskSource TaskSource
	ai         AIProvider
	vcs        VCSProvider
	seen       map[string]struct{}
}

const prDoneLabel = "ai:pr-done"

func NewWorker(cfg config.Config, taskSource TaskSource, ai AIProvider, vcs VCSProvider) *Worker {
	return &Worker{
		cfg:        cfg,
		taskSource: taskSource,
		ai:         ai,
		vcs:        vcs,
		seen:       make(map[string]struct{}),
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	tasks, err := w.taskSource.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("fetch tasks: %w", err)
	}

	if len(tasks) == 0 {
		log.Println("no tasks found")
	} else {
		batches := map[string][]Task{}
		for _, task := range tasks {
			if _, ok := w.seen[task.ID]; ok {
				log.Printf("task %s skipped: already processed in this worker session", task.ID)
				continue
			}
			w.seen[task.ID] = struct{}{}

			repoCfg, ok := w.cfg.Repositories[task.ProjectID]
			if !ok {
				log.Printf("task %s ignored: project %s not mapped", task.ID, task.ProjectID)
				continue
			}
			base := repoCfg.BaseBranch
			if base == "" {
				base = "main"
			}
			key := repoCfg.Repo + "|" + base
			batches[key] = append(batches[key], task)
		}

		if len(batches) == 0 {
			log.Println("no mapped tasks found")
		} else {
			keys := make([]string, 0, len(batches))
			for key := range batches {
				keys = append(keys, key)
			}
			sort.Strings(keys)

			for _, key := range keys {
				parts := strings.SplitN(key, "|", 2)
				repo := RepoTarget{FullName: parts[0], BaseBranch: parts[1]}
				group := batches[key]
				if err := w.processTaskBatch(ctx, repo, group); err != nil {
					log.Printf("tasks %s failed: %v", joinTaskIDs(group), err)
				}
			}
		}
	}

	if err := w.closeMergedPRTasks(ctx); err != nil {
		return fmt.Errorf("close merged pr tasks: %w", err)
	}

	return nil
}

func (w *Worker) closeMergedPRTasks(ctx context.Context) error {
	tasks, err := w.taskSource.FetchByLabel(ctx, prDoneLabel)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}

	for _, task := range tasks {
		prURL, err := w.taskSource.FindPRURL(ctx, task.ID)
		if err != nil {
			log.Printf("task %s pr url lookup failed: %v", task.ID, err)
			continue
		}
		if strings.TrimSpace(prURL) == "" {
			continue
		}

		merged, err := w.vcs.IsPullRequestMerged(ctx, prURL)
		if err != nil {
			log.Printf("task %s pr merged check failed: %v", task.ID, err)
			continue
		}
		if !merged {
			continue
		}

		repoCfg, ok := w.cfg.Repositories[task.ProjectID]
		if !ok {
			log.Printf("task %s close skipped: project %s not mapped", task.ID, task.ProjectID)
			continue
		}
		base := repoCfg.BaseBranch
		if base == "" {
			base = "main"
		}
		repo := RepoTarget{FullName: repoCfg.Repo, BaseBranch: base}

		repoPath, err := w.vcs.PrepareRepo(ctx, repo)
		if err != nil {
			log.Printf("task %s close skipped: prepare repo failed: %v", task.ID, err)
			continue
		}

		versionTag, err := computeLatestTag(ctx, repoPath)
		if err != nil {
			log.Printf("task %s close skipped: latest tag failed: %v", task.ID, err)
			continue
		}

		versionOnly := versionFromTag(versionTag)
		commitMsg := fmt.Sprintf("chore: finalize task %s push-ver:%s", task.ID, versionOnly)
		if err := w.vcs.CreateEmptyCommit(ctx, repoPath, commitMsg); err != nil {
			log.Printf("task %s close skipped: empty commit failed: %v", task.ID, err)
			continue
		}
		if err := w.vcs.Push(ctx, repoPath, repo.BaseBranch); err != nil {
			log.Printf("task %s close skipped: push base failed: %v", task.ID, err)
			continue
		}

		if w.cfg.Worker.CommentOnTask {
			comment := fmt.Sprintf("Task closed automatically: PR merged (%s)\nEmpty commit: %s", prURL, commitMsg)
			if err := w.taskSource.Comment(ctx, task.ID, comment); err != nil {
				log.Printf("task %s close comment failed: %v", task.ID, err)
			}
		}

		if err := w.taskSource.Close(ctx, task.ID); err != nil {
			log.Printf("task %s close after merge failed: %v", task.ID, err)
			continue
		}
	}

	return nil
}

func (w *Worker) processTaskBatch(ctx context.Context, repo RepoTarget, tasks []Task) error {
	if len(tasks) == 0 {
		return nil
	}

	markFailed := func(task *Task) {
		failedLabels := addLabel(
			removeLabel(
				removeLabel(
					removeLabel(task.Labels, w.cfg.TaskSource.Todoist.Label),
					"ai:coding",
				),
				"ai:pr-done",
			),
			"ai:failed",
		)
		if err := w.taskSource.UpdateLabels(ctx, task.ID, failedLabels); err != nil {
			log.Printf("task %s update labels (ai:failed) failed: %v", task.ID, err)
			return
		}
		task.Labels = failedLabels
	}

	markBatchFailed := func() {
		for i := range tasks {
			markFailed(&tasks[i])
		}
	}

	repoPath, err := w.vcs.PrepareRepo(ctx, repo)
	if err != nil {
		markBatchFailed()
		return fmt.Errorf("prepare repo: %w", err)
	}

	branch := branchNameForBatch(tasks)
	if err := w.vcs.CreateBranch(ctx, repoPath, branch, repo.BaseBranch); err != nil {
		markBatchFailed()
		return fmt.Errorf("create branch: %w", err)
	}

	if w.cfg.Worker.DryRun {
		log.Printf("dry-run tasks=%s repo=%s branch=%s", joinTaskIDs(tasks), repo.FullName, branch)
		return nil
	}

	for i := range tasks {
		codingLabels := addLabel(removeLabel(removeLabel(tasks[i].Labels, w.cfg.TaskSource.Todoist.Label), "ai:failed"), "ai:coding")
		if err := w.taskSource.UpdateLabels(ctx, tasks[i].ID, codingLabels); err != nil {
			log.Printf("task %s update labels (ai:coding) failed: %v", tasks[i].ID, err)
		}
		tasks[i].Labels = codingLabels
	}

	batchTask := mergeTasks(tasks)
	aiResult, err := w.ai.Develop(ctx, repoPath, repo.FullName, batchTask)
	for _, task := range tasks {
		w.commentAIResult(ctx, task.ID, aiResult, err)
	}
	if err != nil {
		markBatchFailed()
		return fmt.Errorf("ai develop: %w", err)
	}

	msg := fmt.Sprintf("feat: implement %d todoist tasks (%s)", len(tasks), joinTaskIDs(tasks))
	if hasLabelInBatch(tasks, "@build") {
		versionTag, metaErr := computeLatestTag(ctx, repoPath)
		if metaErr != nil {
			log.Printf("tasks %s build metadata failed: %v", joinTaskIDs(tasks), metaErr)
		} else {
			msg = fmt.Sprintf("%s push-ver:%s", msg, versionTag)
		}
	}
	changed, err := w.vcs.CommitAll(ctx, repoPath, msg)
	if err != nil {
		markBatchFailed()
		return fmt.Errorf("commit changes: %w", err)
	}
	if !changed {
		markBatchFailed()
		return fmt.Errorf("tasks %s produced no changes", joinTaskIDs(tasks))
	}

	if err := w.vcs.Push(ctx, repoPath, branch); err != nil {
		markBatchFailed()
		return fmt.Errorf("push branch: %w", err)
	}

	prTitle := prTitleForBatch(tasks)
	prBody := buildPRBody(tasks, aiResult.Summary)
	prURL, err := w.vcs.OpenPullRequest(ctx, repo.FullName, repo.BaseBranch, branch, prTitle, prBody, w.cfg.VCS.GitHub.Draft)
	if err != nil {
		markBatchFailed()
		return fmt.Errorf("open pr: %w", err)
	}

	for i := range tasks {
		finalLabels := addLabel(removeLabel(tasks[i].Labels, "ai:coding"), "ai:pr-done")
		if err := w.taskSource.UpdateLabels(ctx, tasks[i].ID, finalLabels); err != nil {
			log.Printf("task %s update labels (ai:pr-done) failed: %v", tasks[i].ID, err)
		}
		tasks[i].Labels = finalLabels

		if w.cfg.Worker.CommentOnTask {
			summary := strings.TrimSpace(aiResult.Summary)
			if summary == "" {
				summary = "No summary provided."
			}
			comment := fmt.Sprintf("PR created: %s\nSummary: %s", prURL, summary)
			if err := w.taskSource.Comment(ctx, tasks[i].ID, comment); err != nil {
				log.Printf("task %s comment failed: %v", tasks[i].ID, err)
			}
		}
	}

	log.Printf("tasks %s completed PR=%s", joinTaskIDs(tasks), prURL)
	return nil
}

func addLabel(labels []string, label string) []string {
	for _, existing := range labels {
		if strings.EqualFold(existing, label) {
			return append([]string(nil), labels...)
		}
	}
	out := append([]string(nil), labels...)
	out = append(out, label)
	return out
}

func removeLabel(labels []string, label string) []string {
	out := make([]string, 0, len(labels))
	for _, existing := range labels {
		if strings.EqualFold(existing, label) {
			continue
		}
		out = append(out, existing)
	}
	return out
}

func buildPRBody(tasks []Task, aiSummary string) string {
	body := fmt.Sprintf("Automated implementation for %d Todoist task(s).\n\n", len(tasks))
	body += "Included tasks:\n"
	for _, task := range tasks {
		body += fmt.Sprintf("- %s: %s\n", task.ID, strings.TrimSpace(task.Title))
		if strings.TrimSpace(task.Description) != "" {
			body += "  Description: " + strings.TrimSpace(task.Description) + "\n"
		}
	}
	summary := strings.TrimSpace(aiSummary)
	if summary == "" {
		summary = "No summary provided."
	}
	body += "\nAI Summary:\n"
	body += summary + "\n"
	body += "\n"
	body += "Generated by worker agent (Todoist + Codex)."
	return body
}

func mergeTasks(tasks []Task) Task {
	if len(tasks) == 1 {
		return tasks[0]
	}

	var desc strings.Builder
	desc.WriteString("Batch execution for multiple Todoist tasks.\n\n")
	for _, t := range tasks {
		desc.WriteString("- ID: " + t.ID + "\n")
		desc.WriteString("  Title: " + strings.TrimSpace(t.Title) + "\n")
		if strings.TrimSpace(t.Description) != "" {
			desc.WriteString("  Description: " + strings.TrimSpace(t.Description) + "\n")
		}
		desc.WriteString("\n")
	}

	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return Task{
		ID:          strings.Join(ids, ","),
		Title:       fmt.Sprintf("Batch of %d tasks", len(tasks)),
		Description: strings.TrimSpace(desc.String()),
		ProjectID:   tasks[0].ProjectID,
	}
}

func joinTaskIDs(tasks []Task) string {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return strings.Join(ids, ",")
}

func branchNameForBatch(tasks []Task) string {
	if len(tasks) == 1 {
		return util.BranchName(tasks[0].ID, tasks[0].Title)
	}
	return util.BranchName(tasks[0].ID, fmt.Sprintf("batch-%d-tasks", len(tasks)))
}

func prTitleForBatch(tasks []Task) string {
	if len(tasks) == 1 {
		return fmt.Sprintf("[AI] %s", strings.TrimSpace(tasks[0].Title))
	}
	return fmt.Sprintf("[AI] Batch of %d Todoist tasks", len(tasks))
}

func hasLabelInBatch(tasks []Task, label string) bool {
	for _, task := range tasks {
		for _, existing := range task.Labels {
			if strings.EqualFold(strings.TrimSpace(existing), strings.TrimSpace(label)) {
				return true
			}
		}
	}
	return false
}

func computeLatestTag(ctx context.Context, repoPath string) (string, error) {
	// Refresh tags to compute metadata against latest remote state.
	if fetchRes, err := util.RunCmd(ctx, repoPath, nil, "git", "fetch", "--tags", "--force", "origin"); err != nil {
		return "", err
	} else if fetchRes.ExitCode != 0 {
		return "", fmt.Errorf("git fetch tags failed: %s", strings.TrimSpace(fetchRes.Stderr))
	}

	latestTagRes, err := util.RunCmd(ctx, repoPath, nil, "git", "tag", "--sort=-v:refname")
	if err != nil {
		return "", err
	}
	if latestTagRes.ExitCode != 0 {
		return "", fmt.Errorf("git tag latest failed: %s", strings.TrimSpace(latestTagRes.Stderr))
	}

	latestTag := "none"
	if line := firstNonEmptyLine(latestTagRes.Stdout); line != "" {
		latestTag = line
	}
	return latestTag, nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func versionFromTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.EqualFold(tag, "none") {
		return "0.0.0"
	}

	// Extract SemVer-like core (major.minor.patch), ignoring prefixes/suffixes (e.g. v1.2.3-build.4).
	re := regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+\.[0-9]+\.[0-9]+)(?:[^0-9]|$)`)
	if m := re.FindStringSubmatch(tag); len(m) == 2 {
		return m[1]
	}
	return "0.0.0"
}

func (w *Worker) commentAIResult(ctx context.Context, taskID string, result AIResult, developErr error) {
	summary := strings.TrimSpace(result.Summary)
	if summary == "" {
		if developErr != nil {
			summary = "Execution failed before producing a summary."
		} else {
			summary = "No summary provided."
		}
	}

	var body strings.Builder
	body.WriteString("Summary: ")
	body.WriteString(summary)
	body.WriteString("\n")
	body.WriteString("Files edited:\n")
	if len(result.ChangedFiles) == 0 {
		body.WriteString("- (none)")
	} else {
		for _, path := range result.ChangedFiles {
			body.WriteString("- ")
			body.WriteString(path)
			body.WriteString("\n")
		}
	}

	if err := w.taskSource.Comment(ctx, taskID, strings.TrimSpace(body.String())); err != nil {
		log.Printf("task %s AI output comment failed: %v", taskID, err)
	}
}
