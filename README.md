<h1 align="center"> Welcome to Keploy 👋 </h1>

<p style="text-align:center;" align="center">
  <img align="center" src="https://avatars.githubusercontent.com/u/92252339?s=200&v=4" height="20%" width="20%" />
</p>

<p align="center">
  <a href="CODE_OF_CONDUCT.md" alt="Contributions welcome">
    <img src="https://img.shields.io/badge/Contributions-Welcome-brightgreen?logo=github" /></a>
    
  <a href="https://github.com/keploy/keploy/actions" alt="Tests">
    <img src="https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg" /></a>
    
  <a href="https://goreportcard.com/report/github.com/keploy/keploy" alt="Go Report Card">
    <img src="https://goreportcard.com/badge/github.com/keploy/keploy" /></a>
    
  <a href="https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA" alt="Slack">
    <img src=".github/slack.svg" /></a>
  
  <a href="https://docs.keploy.io" alt="Docs">
    <img src=".github/docs.svg" /></a>
    
  <a href="https://gitpod.io/#https://github.com/keploy/samples-go" alt="Gitpod">
    <img src="https://img.shields.io/badge/Gitpod-ready--to--code-FFB45B?logo=gitpod" /></a>

</p>

# Keploy
Keploy is a functional testing toolkit for developers. It **generates E2E tests for APIs (KTests)** along with **mocks or stubs(KMocks)** by recording real API calls.
KTests can be imported as mocks for consumers and vice-versa.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/how-keploy-works.gif" width="100%"  alt="Generate Test Case from API call"/>

Merge KTests with unit testing libraries(like Go-Test, JUnit..) to track combined test-coverage.

KMocks can also be referenced in existing tests or use anywhere (including any testing framework). KMocks can also be used as tests for the server.   

> Keploy is testing itself with &nbsp;  [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;  without writing many test-cases or data-mocks. 😎

## How it works?
Keploy CLI adds hooks to your kernel system calls that will capture all the incoming and outgoing network interaction of your application. The networks calls will be saved as end-to-end testcase.

Visit [https://docs.keploy.io](https://docs.keploy.io/docs/keploy-explained/how-keploy-works) to read more in detail..


<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="80%"  alt="Generate Test Case from API call"/>

## Features
### Record the network requests
`record` command can be used to attach keploy hooks and capture all the network calls for your application. Steps to follow: 

1. Start your API server.
2. Determine the 
