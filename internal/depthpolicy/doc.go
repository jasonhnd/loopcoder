// Package depthpolicy selects reasoning depth from task difficulty and
// model-supported depths (V090-CRO-005 / #1338).
//
// Rules:
//   - Never default every task to high.
//   - Docs/tiny fixes → low (or lowest available sufficient depth).
//   - Standard implementation → medium.
//   - Architecture/security/migration/ambiguous → high (or xhigh/max when required).
//   - If requested depth is unsupported, pick nearest lower eligible; fail closed
//     only when no supported depth exists.
package depthpolicy
