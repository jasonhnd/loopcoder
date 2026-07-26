// Package nativechild contains provider-native sub-agents under one LoopCoder
// Attempt with aggregated resource accounting (V090-062).
//
// Native children are evidence under the parent Attempt — never WorkItems.
// They cannot own GitHub issues, branches, worktrees, PRs, verification, merge,
// route changes, or terminal truth. Parent cancel joins all descendants.
package nativechild
