# The wsaw bot GitHub App

Scheduled jobs that open pull requests — `chromium.yaml` and
`chromium-debian.yaml` today — open them as a GitHub App, not with
`GITHUB_TOKEN`. A pull request opened with `GITHUB_TOKEN` does not start
other workflows, so CI would not run on the bump until someone pushed to it
by hand. A long-lived personal access token would fix that too, but it is tied
to a person and does not expire; each app token is minted for one run, scoped to
this repository, and expires within an hour.

## Setting it up

An organization owner does this once.

1. **Create the app:** Organization settings → Developer settings → GitHub
   Apps → New GitHub App.
   - Name: `wsaw-bot` (any free name works).
   - Homepage URL: the repository URL.
   - Webhook: untick **Active**. The app only mints tokens.
   - Repository permissions: **Contents** read and write, **Pull requests**
     read and write. Leave everything else at no access.
   - Where can this app be installed: **Only on this account**.
2. **Generate a private key** on the app's page and download the `.pem`.
3. **Install the app** (Install App → the organization) on **only** the
   `wsaw` repository.
4. **Hand it to the repository:**

   ```sh
   gh variable set BOT_APP_CLIENT_ID --body '<client ID from the app page>'
   gh secret set BOT_APP_PRIVATE_KEY < wsaw-bot.*.private-key.pem
   ```

   Then delete the local `.pem`.
5. **Check it:** run the Chromium workflow by hand
   (`gh workflow run chromium.yaml`). The "Mint a GitHub App token" step must
   run, not be skipped.

Without `BOT_APP_CLIENT_ID` the token step is skipped and the jobs fall back to
`GITHUB_TOKEN`: the pull request is still opened, but CI has to be started by
hand.

## Adding another job

Copy the "Mint a GitHub App token" step from `chromium.yaml`, request only the
`permission-*` inputs that job needs, and use
`${{ steps.app.outputs.token || github.token }}` wherever it pushes or calls
`gh`. If the job needs a permission the app does not have yet, add it on the
app's settings page. The installation then has to approve the new permission.
