package utgen

import (
	"html/template"
	"log"
	"os"
)

// HTML template for the report
const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Test Results</title>
    <link href="https://cdnjs.cloudflare.com/ajax/libs/prism/1.23.0/themes/prism-okaidia.min.css" rel="stylesheet" />
    <style>
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            margin: 20px;
        }
        table {
            border-collapse: collapse;
            width: 100%;
            box-shadow: 0 2px 3px rgba(0,0,0,0.1);
        }
        th, td {
            border: 1px solid #ddd;
            text-align: left;
            padding: 8px;
        }
        th {
            background-color: #f2f2f2;
        }
        tr:nth-child(even) {
            background-color: #f9f9f9;
        }
        .status-pass {
            color: green;
        }
        .status-fail {
            color: red;
        }
        pre {
            background-color: #000000 !important;
            color: #ffffff !important;
            padding: 5px;
            border-radius: 5px;
        }
    </style>
</head>
<body>
    <table>
        <tr>
            <th>Status</th>
            <th>Reason</th>
            <th>Exit Code</th>
            <th>Stderr</th>
            <th>Stdout</th>
            <th>Test</th>
        </tr>
        {{range .}}
        <tr>
            <td class="status-{{.Status}}">{{.Status}}</td>
            <td>{{.Reason}}</td>
            <td>{{.ExitCode}}</td>
            <td>{{if .Stderr}}<pre><code class="language-shell">{{.Stderr}}</code></pre>{{else}}&nbsp;{{end}}</td>
            <td>{{if .Stdout}}<pre><code class="language-shell">{{.Stdout}}</code></pre>{{else}}&nbsp;{{end}}</td>
            <td>{{if .Test}}<pre><code class="language-python">{{.Test}}</code></pre>{{else}}&nbsp;{{end}}</td>
        </tr>
        {{end}}
    </table>
    <script src="https://cdnjs.cloudflare.com/ajax/libs/prism/1.23.0/prism.min.js"></script>
</body>
</html>
`

// GenerateReport renders the HTML report with given results and writes it to a file
func GenerateReport(results []TestResult, filePath string) {
	tmpl, err := template.New("report").Parse(htmlTemplate)
	if err != nil {
		log.Fatalf("Error parsing template: %v", err)
	}

	file, err := os.Create(filePath)
	if err != nil {
		log.Fatalf("Error creating file: %v", err)
	}
	defer file.Close()

	err = tmpl.Execute(file, results)
	if err != nil {
		log.Fatalf("Error executing template: %v", err)
	}
}
