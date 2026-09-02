# Getting Google Drive API credentials

Drivel does **not** ship with a shared Google application identity, and there is
no hosted Drivel service. To sync with Drive you create your own **OAuth client
ID and secret** in a Google Cloud project you own; Drivel uses them to talk to
your Drive on your behalf. You own these credentials: treat them as secrets, keep
them out of version control (they are gitignored), and revoke them from the Cloud
Console if they ever leak.

This is a one-time setup, about five minutes, and free — the Drive API's
per-project quotas are generous for personal use. What you produce here is a
`credentials.json` file (or a client ID + secret you paste into `drivel login`);
`drivel login` then exchanges it for a `token.json` scoped to your account.

> **Terminology.** These are *OAuth client credentials* (a client ID + secret),
> not a Google "API key." The Drive API rejects plain API keys for user-data
> access — it requires OAuth. So there is no API-key field to copy; you create an
> **OAuth client ID** of type **Desktop app**.

---

## Step 1 — Create or select a Google Cloud project

1. Sign in at the [Google Cloud Console](https://console.cloud.google.com/) with
   the Google account whose Drive you want to sync (or a separate "developer"
   account — the project owner and the synced account need not be the same).
2. Open the **project picker** in the top bar and click **New Project**.
3. Give it any name (for example `drivel`). A billing account is **not** required
   for the Drive API. Click **Create**.
4. Make sure the new project is the **selected** project in the top bar before
   continuing — every step below is scoped to the active project.

## Step 2 — Enable the Google Drive API

1. Go to **APIs & Services → Library**
   ([direct link](https://console.cloud.google.com/apis/library)).
2. Search for **Google Drive API** and open it.
3. Click **Enable**. (If it already says "Manage," it's enabled.)

Without this, token exchange may succeed but every Drive call returns
`403 accessNotConfigured`.

## Step 3 — Configure the OAuth consent screen

Google requires a consent screen before it will issue any OAuth client. Under
**APIs & Services → OAuth consent screen**
([direct link](https://console.cloud.google.com/apis/credentials/consent)):

1. **User type:**
   - **External** — for a personal `@gmail.com` account. This is the normal
     choice. It does *not* mean "published to the world"; you can keep the app in
     **Testing** status and restrict it to test users you list (below).
   - **Internal** — only available if your account is part of a Google Workspace
     organization. It skips the test-user and verification friction entirely, so
     prefer it if you have it.

2. **App information:** fill in an **app name** (anything, e.g. `drivel`) and pick
   your own email for the **user support email** and, later, the **developer
   contact** email. You do **not** need a homepage, an application privacy policy,
   or an authorized domain — leave those blank. Nobody but you uses this app.

3. **Scopes:** you can **skip** adding scopes on this screen. Drivel requests the
   scope it needs at login time (`drive`, `drive.readonly`, or `drive.file`);
   pre-declaring them here is optional and only affects the consent-screen text.

4. **Test users:** add the Google account(s) whose Drive you will sync. Only
   listed test users can complete login while the app is in Testing status.

5. Leave the app in **Testing** status. You do **not** need to "Publish" it or
   submit it for Google verification for personal use.

## Step 4 — Create the OAuth client ID (Desktop app)

Under **APIs & Services → Credentials**
([direct link](https://console.cloud.google.com/apis/credentials)):

1. Click **Create Credentials → OAuth client ID**.
2. **Application type:** choose **Desktop app**. This is important — Drivel uses
   the installed-app / loopback redirect flow, which the Desktop app type enables.
   (Do **not** pick "Web application"; its redirect-URI rules differ.)
3. Name it (e.g. `drivel-desktop`) and click **Create**.
4. In the dialog, copy the **Client ID** and **Client secret**, or click
   **Download JSON** to save the client-secret file.

You now have everything Drivel needs.

## Step 5 — Feed the credentials to Drivel

Run the wizard:

```sh
drivel login
```

Provide the client ID/secret two ways:

- **Interactively:** paste them when prompted, or place the downloaded JSON at
  `./credentials.json` and the wizard will read the ID/secret from it (press Enter
  to keep the found values).
- **Non-interactively:**

  ```sh
  drivel login -client-id <ID> -client-secret <SECRET> -scope drive
  ```

The wizard writes `credentials.json`, prints an authorization URL, and captures
the OAuth result. It runs a loopback server on port **53682** (rclone's port) to
catch the browser redirect automatically; if the browser can't reach that address
(e.g. inside a container without the port forwarded), paste the full redirect URL
— or just the `code=` value — from the address bar into the prompt. On success it
saves `token.json` and prints the authenticated account.

Then mount with sync enabled:

```sh
drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json -drive-root <folderID>
```

---

## Choosing a scope

`drivel login -scope` (or the interactive menu) picks how much access the token
grants:

| Scope            | Access                                                     | Use when |
|------------------|------------------------------------------------------------|----------|
| `drive`          | Full read/write to all files in your Drive                 | A general sync mount (recommended). |
| `drive.readonly` | Read-only to file metadata and contents                    | You only want inbound sync / a mirror. |
| `drive.file`     | Only files the app itself creates or opens                 | You want Drivel sandboxed to its own files. |

Narrower is safer; `drive` is the most convenient for a two-way mount. To change
scope later, re-run `drivel login` with a different `-scope` — the new consent
replaces the old token.

## Troubleshooting

- **`403 access_denied` / "app is being tested" at consent.** The signed-in
  account isn't in the **Test users** list (Step 3.4). Add it, or use an Internal
  app type if you have Workspace.
- **`403 accessNotConfigured` on Drive calls.** The Drive API isn't enabled for
  the project (Step 2), or you enabled it in a *different* project than the client
  ID belongs to.
- **`redirect_uri_mismatch`.** The OAuth client is the wrong type. It must be
  **Desktop app** (Step 4.2), which permits loopback redirects.
- **Login works, then breaks after ~a week.** Refresh tokens for **unverified,
  Testing-status** apps can expire after ~7 days. Just re-run `drivel login`. To
  avoid it, publish the app (adds a Google review) or use an Internal Workspace
  app.
- **The loopback capture never fires (container/headless).** Forward the port
  (`docker run -p 127.0.0.1:53682:53682 …`) or use the **paste fallback** — copy
  the redirect URL from the browser into the prompt. You can also pass `-port 0`
  to let the OS pick a free port.
- **Leaked a secret?** In **Credentials**, delete the OAuth client (or reset its
  secret) and create a new one. Deleting the client immediately invalidates tokens
  minted from it.

## What Drivel stores, and where

- `credentials.json` — your OAuth **client ID/secret**. Identifies *your project*
  to Google. Secret; gitignored.
- `token.json` — the **access/refresh token** for your account, obtained by the
  login flow. Secret; gitignored.
- `drivel-state.db` — sync bookkeeping (change-feed cursor + echo records). Not a
  credential, but kept outside the backing tree so it isn't synced to Drive.

Drivel bundles no application secrets and collects no analytics or telemetry. All
Drive traffic is directly between your machine and Google, under the app identity
you just created.
