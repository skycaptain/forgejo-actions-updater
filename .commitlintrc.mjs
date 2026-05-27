// Skycaptain: Forgejo Actions Updater
//
// See https://commitlint.js.org/ for usage.
//

/** @type {import('@commitlint/types').UserConfig} */
export default {
  extends: ["@commitlint/config-conventional"],
  ignores: [
    // Skip CI commits
    (message) => message.toLowerCase().includes("[skip ci]"),
    (message) => message.toLowerCase().includes("[ci skip]"),
    // Skip Renovate Bot messages, as dependency names often exceed the 100 character limit
    (message) => message.toLowerCase().includes("(deps): "),
  ],
  rules: {
    // Downgrade the severity of the body-max-line-length rule to a warning, because of
    // https://github.com/conventional-changelog/commitlint/issues/2112
    "body-max-line-length": [1, "always", 100],
    // Encourage 'Ref: ABC-123' format in the footer
    "references-empty": [1, "always"],
  },
};
