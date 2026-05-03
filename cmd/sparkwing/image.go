// `sparkwing image` subcommand. Composite verbs for the gitops-ops
// image-rollout dance that otherwise requires a chain of scripts:
//
//  1. Locate the image entry in kustomization.yaml
//  2. Rewrite newTag:
//  3. git add / commit / push
//  4. argocd app sync
//  5. kubectl rollout status
//  6. kubectl logs -f
//
// Step 1-3 are the core value add: yaml-aware rewrite of one image
// entry out of many, idempotent commit, SHA on stdout. Step 4-6 are
// opt-in (--wait / --tail-logs) and degrade gracefully when the
// underlying tool is missing from PATH -- operators without argocd
// installed still get the gitops commit, just not the sync.
//
// Scope boundary: this verb does NOT build or push images. The
// consumer pipeline that produced --tag owns that. Keeps the surface
// small and the dependency surface tiny (just git + kubectl + argocd
// via os/exec, no new SDK deps).
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	flag "github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"
)

func runImage(args []string) error {
	if handleParentHelp(cmdImage, args) {
		return nil
	}
	if len(args) == 0 {
		PrintHelp(cmdImage, os.Stderr)
		return errors.New("image: subcommand required (rollout)")
	}
	switch args[0] {
	case "rollout":
		return runImageRollout(args[1:])
	default:
		PrintHelp(cmdImage, os.Stderr)
		return fmt.Errorf("image: unknown subcommand %q", args[0])
	}
}

// runImageRollout is the main entry point for `sparkwing image
// rollout`. The flow is:
//
//  1. Parse + validate flags.
//  2. Resolve the gitops repo path (explicit flag > profile hint >
//     ~/code/kikd-gitops default).
//  3. Locate + rewrite the newTag on the matching image entry in
//     kustomization.yaml.
//  4. git add + commit + push (idempotent on no-diff).
//  5. argocd app sync (skip cleanly when argocd isn't on PATH).
//  6. kubectl rollout status (only when --wait; error if kubectl
//     missing).
//  7. kubectl logs -f (only when --tail-logs; same guard).
//
// Any step that has already happened in a prior run is a no-op --
// re-running with the same --tag prints "nothing to commit" and
// still runs the sync + wait stages. That makes the verb safe to
// drop into a pipeline retry loop.
func runImageRollout(args []string) error {
	fs := flag.NewFlagSet(cmdImageRollout.Path, flag.ContinueOnError)
	image := fs.String("image", "", "short image name (matches suffix of ECR URL)")
	tag := fs.String("tag", "", "new tag to write")
	on := fs.String("on", "", "profile name")
	gitopsRepo := fs.String("gitops-repo", "", "gitops repo path (default: ~/code/kikd-gitops)")
	namespace := fs.String("namespace", "sparkwing", "kubernetes namespace for rollout + logs")
	argocdApp := fs.String("argocd-app", "", "argocd app name (default: derived from --image)")
	message := fs.String("message", "", "override the commit message")
	wait := fs.Bool("wait", false, "block until kubectl rollout status completes")
	tailLogs := fs.Bool("tail-logs", false, "tail pod logs after rollout completes")
	dryRun := fs.Bool("dry-run", false, "print the plan without writing, committing, pushing, or syncing")
	if err := parseAndCheck(cmdImageRollout, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}

	// Resolve the gitops repo path. The --gitops-repo flag wins; else
	// fall back to ~/code/kikd-gitops which matches the author's
	// machine. A future profile field (Profile.GitopsRepo) can slot
	// in here without changing callers.
	repoRoot, err := resolveGitopsRepo(*gitopsRepo, *on)
	if err != nil {
		return fmt.Errorf("image rollout: %w", err)
	}

	// kustomization.yaml lives under <repo>/sparkwing/kustomization.yaml
	// in the author's layout. Keep the path explicit here; if we ever
	// need a non-sparkwing subdir the flag can take over.
	kustPath := filepath.Join(repoRoot, "sparkwing", "kustomization.yaml")
	if _, err := os.Stat(kustPath); err != nil {
		return fmt.Errorf("image rollout: %w", err)
	}

	fmt.Fprintf(os.Stdout, "plan:\n")
	fmt.Fprintf(os.Stdout, "  gitops repo : %s\n", repoRoot)
	fmt.Fprintf(os.Stdout, "  kustomize   : %s\n", kustPath)
	fmt.Fprintf(os.Stdout, "  image       : %s\n", *image)
	fmt.Fprintf(os.Stdout, "  new tag     : %s\n", *tag)

	// Locate + rewrite. Returns the matched registry URL (for log
	// output) and whether the newTag actually changed (drives the
	// skip-commit path). Dry-run short-circuits before the write.
	matchedURL, currentTag, err := findImageEntry(kustPath, *image)
	if err != nil {
		return fmt.Errorf("image rollout: %w", err)
	}
	fmt.Fprintf(os.Stdout, "  matched     : %s (currently %s)\n", matchedURL, currentTag)

	if *dryRun {
		fmt.Fprintf(os.Stdout, "  [dry-run] would rewrite newTag -> %s\n", *tag)
		fmt.Fprintf(os.Stdout, "  [dry-run] would commit+push\n")
		planSyncAndWait(*image, *argocdApp, *namespace, *wait, *tailLogs)
		return nil
	}

	if currentTag == *tag {
		fmt.Fprintln(os.Stdout, "  newTag already matches; skipping rewrite")
	} else {
		if err := rewriteImageTag(kustPath, *image, *tag); err != nil {
			return fmt.Errorf("image rollout: %w", err)
		}
		fmt.Fprintf(os.Stdout, "  rewrote newTag -> %s\n", *tag)
	}

	commitMsg := *message
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("chore: bump %s to %s", *image, *tag)
	}

	sha, committed, err := gitCommitAndPush(repoRoot, kustPath, commitMsg)
	if err != nil {
		return fmt.Errorf("image rollout: %w", err)
	}
	if committed {
		fmt.Fprintf(os.Stdout, "  committed   : %s\n", sha)
	} else {
		fmt.Fprintln(os.Stdout, "  committed   : (nothing to commit, tree clean)")
	}

	// ArgoCD sync is optional. Skip cleanly when argocd isn't on PATH
	// so operators without it installed still get the gitops commit.
	app := *argocdApp
	if app == "" {
		app = deriveArgoCDApp(*image)
	}
	if err := maybeArgoCDSync(app); err != nil {
		return fmt.Errorf("image rollout: %w", err)
	}

	// --wait blocks on kubectl rollout status. Require kubectl only
	// when the operator opted in so missing kubectl isn't a hard
	// failure for the pure-gitops path.
	deployName := deriveDeploymentName(*image)
	if *wait {
		if err := kubectlRolloutStatus(deployName, *namespace); err != nil {
			return fmt.Errorf("image rollout: %w", err)
		}
	}
	if *tailLogs {
		if err := kubectlTailLogs(deployName, *namespace); err != nil {
			return fmt.Errorf("image rollout: %w", err)
		}
	}
	return nil
}

// resolveGitopsRepo picks the path that contains sparkwing/kustomization.yaml.
// Explicit flag wins; otherwise fall back to ~/code/kikd-gitops which
// matches the author's machine. profileName is currently unused but
// kept in the signature as a hook for a future per-profile gitops
// repo field.
func resolveGitopsRepo(explicit, profileName string) (string, error) {
	_ = profileName
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve --gitops-repo: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	candidate := filepath.Join(home, "code", "kikd-gitops")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("gitops repo not found at %s (pass --gitops-repo)", candidate)
	}
	return candidate, nil
}

// findImageEntry walks the images: array of a kustomization.yaml
// looking for an entry whose name: ends in "/"+target (suffix match),
// returning the full matched name and the current newTag. Error if
// zero or more than one entry matches.
func findImageEntry(path, target string) (matchedName, currentTag string, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", "", fmt.Errorf("read %s: %w", path, rerr)
	}
	var root yaml.Node
	if uerr := yaml.Unmarshal(data, &root); uerr != nil {
		return "", "", fmt.Errorf("parse %s: %w", path, uerr)
	}
	images, ierr := findImagesSeq(&root)
	if ierr != nil {
		return "", "", fmt.Errorf("%s: %w", path, ierr)
	}
	var matches []*yaml.Node
	var matchedNames []string
	for _, entry := range images.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		name := scalarField(entry, "name")
		if name == "" {
			continue
		}
		if imageNameMatches(name, target) {
			matches = append(matches, entry)
			matchedNames = append(matchedNames, name)
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("no image entry matches %q", target)
	case 1:
		tag := scalarField(matches[0], "newTag")
		return matchedNames[0], tag, nil
	default:
		return "", "", fmt.Errorf("ambiguous --image %q: matches %v", target, matchedNames)
	}
}

// rewriteImageTag writes a new newTag: value onto the image entry in
// kustomization.yaml whose name: ends with target. Preserves the
// rest of the file's shape (comments, unrelated entries).
func rewriteImageTag(path, target, newTag string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	images, err := findImagesSeq(&root)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var matched *yaml.Node
	for _, entry := range images.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		name := scalarField(entry, "name")
		if imageNameMatches(name, target) {
			if matched != nil {
				return fmt.Errorf("ambiguous --image %q", target)
			}
			matched = entry
		}
	}
	if matched == nil {
		return fmt.Errorf("no image entry matches %q", target)
	}
	if err := setScalarField(matched, "newTag", newTag); err != nil {
		return fmt.Errorf("set newTag: %w", err)
	}
	// Re-marshal with 2-space indent to match the existing file style.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close encoder: %w", err)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// findImagesSeq dives through the top-level DocumentNode + MappingNode
// to find the "images" sequence. Returns a typed error when the key
// is missing or has the wrong shape so the caller can surface "this
// file doesn't look like a kustomization.yaml with an images block".
func findImagesSeq(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("empty or malformed yaml document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, errors.New("top-level yaml is not a mapping")
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i]
		val := doc.Content[i+1]
		if key.Value == "images" {
			if val.Kind != yaml.SequenceNode {
				return nil, errors.New("images: is not a sequence")
			}
			return val, nil
		}
	}
	return nil, errors.New("no images: block found")
}

// scalarField pulls the string value of a scalar field from a
// MappingNode, or "" if the key is absent / non-scalar.
func scalarField(m *yaml.Node, key string) string {
	if m.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1].Value
		}
	}
	return ""
}

// setScalarField overwrites (or appends) a scalar key/value pair on a
// MappingNode. Creating a missing key keeps the rewrite honest on
// entries that had only name: before (no-op today in practice since
// every entry in our kustomizations carries newTag, but it's the
// right semantics for yaml manipulation).
func setScalarField(m *yaml.Node, key, value string) error {
	if m.Kind != yaml.MappingNode {
		return errors.New("setScalarField: not a mapping")
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = value
			m.Content[i+1].Tag = "!!str"
			return nil
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str"},
	)
	return nil
}

// imageNameMatches returns true when the registry URL in the
// kustomization entry ends with "/"+target. The leading "/" guards
// against partial-word matches (so --image runner doesn't match
// /sparkwing-runner-extra).
func imageNameMatches(fullName, target string) bool {
	if fullName == "" || target == "" {
		return false
	}
	if fullName == target {
		return true
	}
	return strings.HasSuffix(fullName, "/"+target)
}

// gitCommitAndPush is idempotent on no-diff: after `git add`, if the
// working tree is clean vs HEAD for kustPath, it returns committed=false
// and the current HEAD SHA so callers can keep going. Uses the
// package-shared runGit helper (defined in push.go).
func gitCommitAndPush(repoRoot, kustPath, message string) (sha string, committed bool, err error) {
	relPath, rerr := filepath.Rel(repoRoot, kustPath)
	if rerr != nil {
		return "", false, fmt.Errorf("relpath: %w", rerr)
	}
	if _, aerr := runGit(repoRoot, "add", relPath); aerr != nil {
		return "", false, fmt.Errorf("%w", aerr)
	}
	// `git diff --cached --quiet` exits non-zero when there are staged
	// changes; exit 0 means nothing to commit. Use exec directly so we
	// can inspect the exit code without the runGit error-wrap layer.
	cmd := exec.Command("git", "-C", repoRoot, "diff", "--cached", "--quiet")
	if cerr := cmd.Run(); cerr == nil {
		// Clean index -> no commit needed. Return HEAD as the SHA so
		// downstream steps still have a commit to reference.
		out, herr := runGit(repoRoot, "rev-parse", "HEAD")
		if herr != nil {
			return "", false, fmt.Errorf("%w", herr)
		}
		return strings.TrimSpace(out), false, nil
	}
	if _, cerr := runGit(repoRoot, "commit", "-m", message); cerr != nil {
		return "", false, fmt.Errorf("%w", cerr)
	}
	shaOut, rerr := runGit(repoRoot, "rev-parse", "HEAD")
	if rerr != nil {
		return "", false, fmt.Errorf("%w", rerr)
	}
	sha = strings.TrimSpace(shaOut)
	if _, perr := runGit(repoRoot, "push"); perr != nil {
		return "", false, fmt.Errorf("%w", perr)
	}
	return sha, true, nil
}

// maybeArgoCDSync invokes `argocd app sync <app>` if argocd is on
// PATH; otherwise prints a skip notice and returns nil. This keeps
// operators without argocd installed from hitting a hard failure in
// the rollout path.
func maybeArgoCDSync(app string) error {
	if _, err := exec.LookPath("argocd"); err != nil {
		fmt.Fprintln(os.Stdout, "  argocd not on PATH, skipping sync")
		return nil
	}
	fmt.Fprintf(os.Stdout, "  argocd sync : %s\n", app)
	cmd := exec.Command("argocd", "app", "sync", app)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("argocd app sync %s: %w", app, err)
	}
	return nil
}

// kubectlRolloutStatus blocks until `kubectl rollout status
// deployment/<name> -n <ns>` returns. Errors cleanly when kubectl
// isn't on PATH so operators know why --wait failed.
func kubectlRolloutStatus(deploy, namespace string) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl not on PATH; --wait requires kubectl")
	}
	fmt.Fprintf(os.Stdout, "  kubectl rollout status deployment/%s -n %s\n", deploy, namespace)
	cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+deploy, "-n", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl rollout status: %w", err)
	}
	return nil
}

// kubectlTailLogs runs `kubectl logs -f -l app=<name> -n <ns>` and
// inherits stdin/stdout/stderr so ctrl-c terminates the child
// cleanly. Blocks until the user interrupts.
func kubectlTailLogs(deploy, namespace string) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl not on PATH; --tail-logs requires kubectl")
	}
	fmt.Fprintf(os.Stdout, "  kubectl logs -f -l app=%s -n %s (ctrl-c to stop)\n", deploy, namespace)
	cmd := exec.Command("kubectl", "logs", "-f", "-l", "app="+deploy, "-n", namespace)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl logs: %w", err)
	}
	return nil
}

// deriveDeploymentName maps image -> Deployment name. The author's
// convention is 1:1 (image "sparkwing-runner" -> Deployment
// "sparkwing-runner"); --argocd-app / future flags can extend this
// if the convention drifts.
func deriveDeploymentName(image string) string {
	return image
}

// deriveArgoCDApp maps image -> ArgoCD app. Convention: any
// "sparkwing*" image lives inside the single "sparkwing" app, since
// one Application pulls the whole kustomize overlay. Non-sparkwing
// images fall back to the image name itself.
func deriveArgoCDApp(image string) string {
	if strings.HasPrefix(image, "sparkwing") {
		return "sparkwing"
	}
	return image
}

// planSyncAndWait prints the would-happen sync+wait lines for
// --dry-run output. Keeps the dry-run plan faithful to the post-
// commit branch below.
func planSyncAndWait(image, argocdApp, namespace string, wait, tailLogs bool) {
	app := argocdApp
	if app == "" {
		app = deriveArgoCDApp(image)
	}
	if _, err := exec.LookPath("argocd"); err != nil {
		fmt.Fprintln(os.Stdout, "  [dry-run] argocd not on PATH; would skip sync")
	} else {
		fmt.Fprintf(os.Stdout, "  [dry-run] would argocd app sync %s\n", app)
	}
	if wait {
		fmt.Fprintf(os.Stdout, "  [dry-run] would kubectl rollout status deployment/%s -n %s\n",
			deriveDeploymentName(image), namespace)
	}
	if tailLogs {
		fmt.Fprintf(os.Stdout, "  [dry-run] would kubectl logs -f -l app=%s -n %s\n",
			deriveDeploymentName(image), namespace)
	}
}
