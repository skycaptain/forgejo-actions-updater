// Skycaptain: Forgejo Actions Updater
//
// See https://semantic-release.gitbook.io for usage.
//

// RELEASE BUSTER: 0

/** @type {import('semantic-release').GlobalConfig} */
export default {
  plugins: [
    "@semantic-release/commit-analyzer",
    "@semantic-release/release-notes-generator",
    "@semantic-release/github",
  ],
  preset: "conventionalcommits",
};
