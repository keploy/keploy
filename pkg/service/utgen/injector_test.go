package utgen

import (
	"strings"
	"testing"
)

func TestUpdateTypeScriptImports(t *testing.T) {
	injector := &Injector{}

	tests := []struct {
		name         string
		content      string
		newImports   []string
		expected     string
		expectedDiff int
	}{
		{
			name: "No new imports",
			content: `import { isEven } from './math';

function calculate() {
    return isEven(2);
}`,
			newImports: []string{},
			expected: `import { isEven } from './math';

function calculate() {
    return isEven(2);
}`,
			expectedDiff: 0,
		},
		{
			name: "New imports with duplicates",
			content: `import { isEven } from './math';

function calculate() {
    return isEven(2);
}`,
			newImports: []string{
				"import { isEven } from './math';", // Duplicate
				"import { divide } from './math';", // New import
			},
			expected: `import { isEven } from './math';
import { divide } from './math';

function calculate() {
    return isEven(2);
}`,
			expectedDiff: 1,
		},
		{
			name: "No existing imports, adding new imports",
			content: `function calculate() {
    return sum(2, 3);
}`,
			newImports: []string{
				"import { sum } from './math';",
			},
			expected: `import { sum } from './math';
function calculate() {
    return sum(2, 3);
}`,
			expectedDiff: 1,
		},
		{
			name: "Adding imports to content with no imports",
			content: `/* This is a comment */
function calculate() {
    return 42;
}`,
			newImports: []string{
				"import { sqrt } from './math';",
			},
			expected: `import { sqrt } from './math';
/* This is a comment */
function calculate() {
    return 42;
}`,
			expectedDiff: 1,
		},
		{
			name: "Handling empty lines and comments",
			content: `import { isEven } from './math';
// A comment explaining the code
function calculate() {
    return isEven(2);
}`,
			newImports: []string{
				"import { divide } from './math';",
				"import { multiply } from './utils';",
			},
			expected: `import { isEven } from './math';
import { divide } from './math';
import { multiply } from './utils';
// A comment explaining the code
function calculate() {
    return isEven(2);
}`,
			expectedDiff: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updatedContent, diff, err := injector.updateTypeScriptImports(tt.content, tt.newImports)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if strings.TrimSpace(updatedContent) != strings.TrimSpace(tt.expected) {
				t.Errorf("Expected:\n%s\nGot:\n%s", tt.expected, updatedContent)
			}
			if diff != tt.expectedDiff {
				t.Errorf("Expected diff: %d, Got: %d", tt.expectedDiff, diff)
			}
		})
	}
}

func TestUpdateJavaScriptImports(t *testing.T) {
	injector := &Injector{}

	tests := []struct {
		name         string
		content      string
		newImports   []string
		expected     string
		expectedDiff int
	}{
		{
			name: "No new imports",
			content: `import { isEven } from './math';
const { sum } = require('./utils');

function calculate() {
    return sum(2, 3);
}`,
			newImports: []string{},
			expected: `import { isEven } from './math';
const { sum } = require('./utils');

function calculate() {
    return sum(2, 3);
}`,
			expectedDiff: 0,
		},
		{
			name: "New imports with duplicates",
			content: `import { isEven } from './math';
const { sum } = require('./utils');

function calculate() {
    return sum(2, 3);
}`,
			newImports: []string{
				"const { sum } = require('./utils');",   // Duplicate
				"const { divide } = require('./math');", // New import
			},
			expected: `import { isEven } from './math';
const { sum } = require('./utils');
const { divide } = require('./math');

function calculate() {
    return sum(2, 3);
}`,
			expectedDiff: 1,
		},
		{
			name: "No existing imports, adding new imports",
			content: `function calculate() {
    return sum(2, 3);
}`,
			newImports: []string{
				"import { sum } from './math';",
			},
			expected: `import { sum } from './math';
function calculate() {
    return sum(2, 3);
}`,
			expectedDiff: 1,
		},
		{
			name: "Adding imports to content with no imports",
			content: `/* This is a comment */
function calculate() {
    return 42;
}`,
			newImports: []string{
				"import { sqrt } from './math';",
			},
			expected: `import { sqrt } from './math';
/* This is a comment */
function calculate() {
    return 42;
}`,
			expectedDiff: 1,
		},
		{
			name: "Handling empty lines and comments",
			content: `import { isEven } from './math';
// A comment explaining the code
const { sum } = require('./utils');

function calculate() {
    return sum(2, 3);
}`,
			newImports: []string{
				"import { divide } from './math';",
				"const { multiply } = require('./utils');",
			},
			expected: `import { isEven } from './math';
// A comment explaining the code
const { sum } = require('./utils');
import { divide } from './math';
const { multiply } = require('./utils');

function calculate() {
    return sum(2, 3);
}`,
			expectedDiff: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updatedContent, diff, err := injector.updateJavaScriptImports(tt.content, tt.newImports)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if strings.TrimSpace(updatedContent) != strings.TrimSpace(tt.expected) {
				t.Errorf("Expected:\n%s\nGot:\n%s", tt.expected, updatedContent)
			}
			if diff != tt.expectedDiff {
				t.Errorf("Expected diff: %d, Got: %d", tt.expectedDiff, diff)
			}
		})
	}
}

func TestUpdatePythonImports(t *testing.T) {
	injector := &Injector{}

	tests := []struct {
		name         string
		content      string
		newImports   []string
		expected     string
		expectedDiff int
	}{
		{
			name: "No new imports",
			content: `from math import sqrt

def func():
    return sqrt(4)
`,
			newImports: []string{},
			expected: `from math import sqrt

def func():
    return sqrt(4)
`,
			expectedDiff: 0,
		},
		{
			name: "New imports with duplicates",
			content: `from math import sqrt

def func():
    return sqrt(4)
`,
			newImports: []string{
				"from math import sqrt",   // Duplicate
				"from math import divide", // New import
			},
			expected: `from math import sqrt
from math import divide

def func():
    return sqrt(4)
`,
			expectedDiff: 1,
		},
		{
			name: "No existing imports, adding new imports",
			content: `def func():
    return sum(2, 3)
`,
			newImports: []string{
				"import os",
			},
			expected: `import os
def func():
    return sum(2, 3)
`,
			expectedDiff: 1,
		},
		{
			name: "Adding imports to content with no imports",
			content: `# This is a comment
def func():
    return sum(2, 3)
`,
			newImports: []string{
				"import os",
			},
			expected: `import os
# This is a comment
def func():
    return sum(2, 3)
`,
			expectedDiff: 1,
		},
		{
			name: "Handling empty lines and comments",
			content: `from math import sqrt
# A comment explaining the code
from random import randint # checking coverage for file - do not remove

def func():
    return randint(0, sqrt(4))
`,
			newImports: []string{
				"from math import ceil",
				"from random import randint",
			},
			expected: `from math import sqrt
# A comment explaining the code
from random import randint # checking coverage for file - do not remove
from math import ceil

def func():
    return randint(0, sqrt(4))
`,
			expectedDiff: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updatedContent, diff, err := injector.updatePythonImports(tt.content, tt.newImports)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Compare the updated content with the expected content
			if strings.TrimSpace(updatedContent) != strings.TrimSpace(tt.expected) {
				t.Errorf("Expected:\n%s\nGot:\n%s", tt.expected, updatedContent)
			}

			// Compare the difference (number of lines added)
			if diff != tt.expectedDiff {
				t.Errorf("Expected diff: %d, Got: %d", tt.expectedDiff, diff)
			}
		})
	}
}
