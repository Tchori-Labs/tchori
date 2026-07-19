# Security Policy

## Supported versions

tchori is currently `0.1.0-dev`: a pre-MVP project under active development.
During pre-1.0 development, security fixes are made only on the latest release
or on `main`, as appropriate. There are no LTS, support-window, or backport
guarantees.

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub private vulnerability
reporting: open the repository's **Security** tab, choose **Report a
vulnerability**, and submit a private GitHub Security Advisory.

Do **not** disclose a suspected vulnerability in a public GitHub issue, pull
request, discussion, or other public channel. If a board-approved private
security contact is documented in the future, it may be used as a fallback when
GitHub private vulnerability reporting is unavailable.

> [!IMPORTANT]
> **Maintainer action required:** GitHub private vulnerability reporting is not
> currently enabled, and no board-approved private security contact is
> documented. Maintainers must enable private vulnerability reporting and/or
> designate and document a private fallback contact before this policy is fully
> operative. Until then, do not publish vulnerability details through a public
> repository channel.

## What to include

A useful report should include, when available:

- the affected tchori version or commit;
- the affected component or command;
- clear reproduction steps or a proof of concept;
- expected and actual behavior;
- an assessment of impact and exploitability; and
- any suggested remediation or mitigating controls.

Please keep vulnerability details private while maintainers assess and
coordinate the report.

## Response expectations

This is a pre-MVP, agent-operated project. Acknowledgment, assessment, status
updates, and remediation are handled on a best-effort basis and are subject to
maintainer confirmation. The project does not promise a fixed response or fix
timeline. Maintainers will coordinate disclosure privately when an operative
private channel is available.

## Governance and automated agents

Automated agents must identify themselves as automated in every interaction.
They may assist with triage, investigation, and preparing a fix, but
vulnerability-disclosure decisions remain subject to human/board approval.
CODEOWNERS review is the apply gate: agents do not approve or merge their own
work. Releases require board sign-off, and agents do not tag or publish a
release themselves.
