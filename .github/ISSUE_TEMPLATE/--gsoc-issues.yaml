name: GSoC Issue report
description: Create a Issue report to help us improve Keploy
title: "[gsoc]: "
labels: [gsoc]
body:
- type: markdown
  attributes:
    value: |
      Thank you for taking the time.
- type: checkboxes
  attributes:
    label: Is there an existing issue for this?
    description: Please search to see if an issue already exists for the you encountered
    options:
    - label: I have searched the existing issues
      required: true
- type: textarea
  attributes:
    label: Steps to reproduce
    description: Add steps to reproduce this behaviour, include console or network logs and screenshots
    placeholder: |
      1. Go to '...'
      2. Click on '....'
      3. Scroll down to '....'
      4. See error
  validations:
    required: true
- type: dropdown
  id: repo
  attributes:
    label: Repository
    options:
      - Keploy
      - Java-SDK
      - Samples-Java
      - Typescript-SDK
      - Samples-Typescript
      - Samples-Go
  validations:
    required: true