package agent

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
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
		return nil
	}

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
		return nil
	}

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
					"Coding",
				),
				"PR Opened",
			),
			"Failed",
		)
		if err := w.taskSource.UpdateLabels(ctx, task.ID, failedLabels); err != nil {
			log.Printf("task %s update labels (Failed) failed: %v", task.ID, err)
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
		codingLabels := addLabel(removeLabel(removeLabel(tasks[i].Labels, w.cfg.TaskSource.Todoist.Label), "Failed"), "Coding")
		if err := w.taskSource.UpdateLabels(ctx, tasks[i].ID, codingLabels); err != nil {
			log.Printf("task %s update labels (Coding) failed: %v", tasks[i].ID, err)
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
		versionTag, nextBuild, metaErr := computeBuildMetadata(ctx, repoPath)
		if metaErr != nil {
			log.Printf("tasks %s build metadata failed: %v", joinTaskIDs(tasks), metaErr)
		} else {
			msg = fmt.Sprintf("%s push-ver:%s push-build:%d", msg, versionTag, nextBuild)
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
	prBody := buildPRBody(tasks)
	prURL, err := w.vcs.OpenPullRequest(ctx, repo.FullName, repo.BaseBranch, branch, prTitle, prBody, w.cfg.VCS.GitHub.Draft)
	if err != nil {
		markBatchFailed()
		return fmt.Errorf("open pr: %w", err)
	}

	for i := range tasks {
		finalLabels := addLabel(removeLabel(tasks[i].Labels, "Coding"), "PR Opened")
		if err := w.taskSource.UpdateLabels(ctx, tasks[i].ID, finalLabels); err != nil {
			log.Printf("task %s update labels (PR Opened) failed: %v", tasks[i].ID, err)
		}
		tasks[i].Labels = finalLabels

		if w.cfg.Worker.CommentOnTask {
			comment := fmt.Sprintf("PR created: %s", prURL)
			if err := w.taskSource.Comment(ctx, tasks[i].ID, comment); err != nil {
				log.Printf("task %s comment failed: %v", tasks[i].ID, err)
			}
		}

		if w.cfg.Worker.CloseTaskOnPR {
			if err := w.taskSource.Close(ctx, tasks[i].ID); err != nil {
				log.Printf("task %s close failed: %v", tasks[i].ID, err)
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

func buildPRBody(tasks []Task) string {
	body := fmt.Sprintf("Automated implementation for %d Todoist task(s).\n\n", len(tasks))
	body += "Included tasks:\n"
	for _, task := range tasks {
		body += fmt.Sprintf("- %s: %s\n", task.ID, strings.TrimSpace(task.Title))
		if strings.TrimSpace(task.Description) != "" {
			body += "  Description: " + strings.TrimSpace(task.Description) + "\n"
		}
	}
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

func computeBuildMetadata(ctx context.Context, repoPath string) (string, int, error) {
	// Refresh tags to compute build metadata against latest remote state.
	if fetchRes, err := util.RunCmd(ctx, repoPath, nil, "git", "fetch", "--tags", "--force", "origin"); err != nil {
		return "", 0, err
	} else if fetchRes.ExitCode != 0 {
		return "", 0, fmt.Errorf("git fetch tags failed: %s", strings.TrimSpace(fetchRes.Stderr))
	}

	latestTagRes, err := util.RunCmd(ctx, repoPath, nil, "git", "tag", "--sort=-v:refname")
	if err != nil {
		return "", 0, err
	}
	if latestTagRes.ExitCode != 0 {
		return "", 0, fmt.Errorf("git tag latest failed: %s", strings.TrimSpace(latestTagRes.Stderr))
	}

	allTagsRes, err := util.RunCmd(ctx, repoPath, nil, "git", "tag", "--list")
	if err != nil {
		return "", 0, err
	}
	if allTagsRes.ExitCode != 0 {
		return "", 0, fmt.Errorf("git tag list failed: %s", strings.TrimSpace(allTagsRes.Stderr))
	}

	latestTag := "none"
	if line := firstNonEmptyLine(latestTagRes.Stdout); line != "" {
		latestTag = line
	}

	lastBuild := highestBuildFromTags(allTagsRes.Stdout)
	return latestTag, lastBuild + 1, nil
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

func highestBuildFromTags(tagsRaw string) int {
	lines := strings.Split(tagsRaw, "\n")
	maxBuild := 0

	reBuild := regexp.MustCompile(`(?i)build[-_]?([0-9]+)`)
	reTrailing := regexp.MustCompile(`([0-9]+)$`)

	for _, tag := range lines {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		build := 0

		if m := reBuild.FindStringSubmatch(tag); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				build = n
			}
		} else if m := reTrailing.FindStringSubmatch(tag); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				build = n
			}
		}

		if build > maxBuild {
			maxBuild = build
		}
	}
	return maxBuild
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
