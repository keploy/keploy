# Contributing to Keploy

Thank you for your interest in Keploy and for taking the time to contribute to this project. 🙌 Keploy is a project by developers for developers and there are a lot of ways you can contribute.

If you don't know where to start contributing, ask us on our [Slack channel](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA).

## Code of conduct

Contributors are expected to adhere to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Prerequisites for the contributors

Contributors should have knowledge of git, go, and markdown for most projects since the project work heavily depends on them.
We encourage Contributors to set up Keploy for local development and play around with the code and tests to get more comfortable with the project. 

Sections

- <a name="contributing"> General Contribution Flow</a>
  - <a name="#commit-signing">Developer Certificate of Origin</a>
- <a name="contributing-keploy">Keploy Contribution Flow</a>
  - <a name="keploy-server">Keploy Server</a>
  - <a name="keploy-docs">Keploy Documentation</a>
  - <a name="keploy-sdks">Keploy SDK Integration</a>

# <a name="contributing">General Contribution Flow</a>

## <a name="commit-signing">Signing-off on Commits (Developer Certificate of Origin)</a>

To contribute to this project, you must agree to the Developer Certificate of
Origin (DCO) for each commit you make. The DCO is a simple statement that you,
as a contributor, have the legal right to make the contribution.

See the [DCO](https://developercertificate.org) file for the full text of what you must agree to
and how it works [here](https://github.com/probot/dco#how-it-works).
To signify that you agree to the DCO for contributions, you simply add a line to each of your
git commit messages:

```
Signed-off-by: Jane Smith <jane.smith@example.com>
```

In most cases, you can add this signoff to your commit automatically with the
`-s` or `--signoff` flag to `git commit`. You must use your real name and a reachable email
address (sorry, no pseudonyms or anonymous contributions). An example of signing off on a commit:

```
$ commit -s -m “my commit message w/signoff”
```

To ensure all your commits are signed, you may choose to add this alias to your global `.gitconfig`:

_~/.gitconfig_

```
[alias]
  amend = commit -s --amend
  cm = commit -s -m
  commit = commit -s
```

# How to contribute ?

We encourage contributions from the community.

**Create a [GitHub issue](https://github.com/keploy/keploy/issues) for any changes beyond typos and small fixes.**

We review GitHub issues and PRs on a regular schedule.

To ensure that each change is relevant and properly peer reviewed, please adhere to best practices for open-source contributions.
This means that if you are outside the Keploy organization, you must fork the repository and create PRs from branches on your own fork.
The README in GitHub's [first-contributions repo](https://github.com/firstcontributions/first-contributions) provides an example.

## ## How to set up the docs website locally?

1. Fork the repository

<br/>

2. Clone the repository with the following command. Replace the <GITHUB_USERNAME> with your username

```sh
git clone https://github.com/<GITHUB_USERNAME>/keploy.git
```

<br/>

3. Go into the directory containing the project and edit the changes.


When we merge your PR, a new build automatically occurs and your changes publish to [https://keploy.io](https://github.com/keploy/keploy).

## <a name="contributing-keploy">Keploy Contribution Flow</a>

Keploy is written in `Go` (Golang) and leverages Go Modules. Relevant coding style guidelines are the [Go Code Review Comments](https://code.google.com/p/go-wiki/wiki/CodeReviewComments) and the _Formatting and style_ section of Peter Bourgon's [Go: Best
Practices for Production Environments](https://peter.bourgon.org/go-in-production/#formatting-and-style).

There are many ways in which you can contribute to Keploy.

###  <a name="keploy-server">Keploy Server</a>

#### Report a Bug
Report all issues through GitHub Issues using the [Report a Bug](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=bug_report.md&title=) template.
To help resolve your issue as quickly as possible, read the template and provide all the requested information.

#### Feature request
We welcome all feature requests, whether it's to add new functionality to an existing extension or to offer an idea for a brand new extension.
File your feature request through GitHub Issues using the [Feature Request](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=feature_request.md&title=) template.

#### Close a Bug
We welcome contributions that help make keploy bug free & improve the experience of our users. You can also find issues tagged [Good First Issues](https://github.com/keploy/keploy/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

###  <a name="keploy-docs">Keploy Documentation</a>

The Keploy documentation site uses Docusaurus 2, which is a static website generator, you can make changes locally without previewing them in the browser.

In the process of shipping features quickly, we may forget to keep our docs up to date. You can help by suggesting improvements to our documentation using the [Documentation Improvement](https://github.com/keploy/docs/issues) template. 

Please refer to [Keploy Docs Contributing Guide](https://github.com/keploy/docs/blob/main/CONTRIBUTING.md#-how-to-set-up-the-docs-website-locally) for setting up your development environment and the follow [Keploy Style Guide](https://github.com/keploy/docs/blob/main/STYLE.md).

###  <a name="keploy-sdks">Keploy SDKs</a>

Keploy provides stable support for ``Go``, ``Java``  and ``Ts`` language based applications. 

- [x] [Go SDK](https://github.com/keploy/go-sdk)
- [x] [Java SDK](https://github.com/keploy/java-sdk)
- [x] [TypeScript SDK](https://github.com/keploy/typescript-sdk)

Every SDKs support the popular and common Routers and Databases.


# Contact

Feel free to join [slack](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA) to start a conversation with us.
