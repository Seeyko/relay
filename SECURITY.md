# Security

Please report vulnerabilities privately through GitHub's "Report a vulnerability" button
(Security tab of this repository) rather than in a public issue.

Operational notes:

- Anyone holding `RELAY_API_TOKEN` can publish on every connected account. Use a long random value and HTTPS.
- Account credentials are encrypted at rest with `RELAY_ENCRYPTION_KEY`; keep it out of the database backups.
- Files under `/media/` are public by design (random ids), because some networks download media from Relay.
  Do not upload anything private.
