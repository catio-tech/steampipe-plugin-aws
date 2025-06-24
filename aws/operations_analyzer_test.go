package aws

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAnalyzeWellArchitectedLensReview(t *testing.T) {
	table := tableAwsWellArchitectedLensReview(context.Background())

	operations, err := AnalyzeTableOperations(context.Background(), table)
	if err != nil {
		t.Fatalf("AnalyzeTableOperations failed: %v", err)
	}

	fmt.Println("Discovered AWS Operations for aws_wellarchitected_lens_review:")
	for _, op := range operations {
		fmt.Println("-", op)
	}

	expected := []string{
		"wellarchitected:GetLensReview",
		"wellarchitected:GetWorkload",
		"wellarchitected:ListLensReviews",
		"wellarchitected:ListWorkloads",
	}

	assert.ElementsMatch(t, expected, operations, "The discovered operations should match the expected list.")
}

func TestAnalyzeAccessAnalyzer(t *testing.T) {
	table := tableAwsAccessAnalyzer(context.Background())

	operations, err := AnalyzeTableOperations(context.Background(), table)
	if err != nil {
		t.Fatalf("AnalyzeTableOperations failed: %v", err)
	}

	fmt.Println("Discovered AWS Operations for aws_accessanalyzer_analyzer:")
	for _, op := range operations {
		fmt.Println("-", op)
	}

	expected := []string{
		"access-analyzer:GetAnalyzer",
		"access-analyzer:ListAnalyzers", 
		"access-analyzer:ListFindings",
	}

	assert.ElementsMatch(t, expected, operations, "The discovered operations should match the expected list.")
}

func TestAnalyzeSQLQuery(t *testing.T) {
	tests := []struct {
		name           string
		sqlQuery       string
		expectedTables []string
		expectedOps    map[string][]string
	}{
		{
			name:           "Simple SELECT query",
			sqlQuery:       "SELECT * FROM aws_wellarchitected_lens_review WHERE workload_id = 'test'",
			expectedTables: []string{"aws_wellarchitected_lens_review"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_lens_review": {
					"wellarchitected:GetLensReview",
					"wellarchitected:GetWorkload",
					"wellarchitected:ListLensReviews",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:           "Query with schema prefix",
			sqlQuery:       "SELECT * FROM aws.aws_wellarchitected_workload",
			expectedTables: []string{"aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:     "Query with JOIN",
			sqlQuery: "SELECT w.workload_name, lr.lens_name FROM aws_wellarchitected_workload w JOIN aws_wellarchitected_lens_review lr ON w.workload_id = lr.workload_id",
			expectedTables: []string{"aws_wellarchitected_lens_review", "aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
				"aws_wellarchitected_lens_review": {
					"wellarchitected:GetLensReview",
					"wellarchitected:GetWorkload",
					"wellarchitected:ListLensReviews",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:           "Query with non-AWS table (should be filtered out)",
			sqlQuery:       "SELECT * FROM aws_wellarchitected_workload w JOIN some_other_table t ON w.id = t.id",
			expectedTables: []string{"aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:           "Query with unknown AWS table (should be skipped)",
			sqlQuery:       "SELECT * FROM aws_unknown_table",
			expectedTables: []string{}, // No tables should be processed since aws_unknown_table is not in our mapping
			expectedOps:    map[string][]string{},
		},
		{
			name:     "Complex query with subquery",
			sqlQuery: "SELECT * FROM aws_wellarchitected_workload WHERE workload_id IN (SELECT workload_id FROM aws_wellarchitected_lens_review WHERE lens_name = 'test')",
			expectedTables: []string{"aws_wellarchitected_lens_review", "aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
				"aws_wellarchitected_lens_review": {
					"wellarchitected:GetLensReview",
					"wellarchitected:GetWorkload",
					"wellarchitected:ListLensReviews",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:     "PostgreSQL CTE (Common Table Expression)",
			sqlQuery: "WITH workload_data AS (SELECT workload_id, workload_name FROM aws_wellarchitected_workload WHERE workload_id = 'test') SELECT lr.* FROM aws_wellarchitected_lens_review lr JOIN workload_data wd ON lr.workload_id = wd.workload_id",
			expectedTables: []string{"aws_wellarchitected_lens_review", "aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
				"aws_wellarchitected_lens_review": {
					"wellarchitected:GetLensReview",
					"wellarchitected:GetWorkload",
					"wellarchitected:ListLensReviews",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:     "PostgreSQL advanced features (window functions, nested CTEs)",
			sqlQuery: `
				WITH RECURSIVE workload_hierarchy AS (
					SELECT workload_id, workload_name, 1 as level 
					FROM aws_wellarchitected_workload 
					WHERE workload_id = 'root'
					UNION ALL
					SELECT w.workload_id, w.workload_name, wh.level + 1
					FROM aws_wellarchitected_workload w
					JOIN workload_hierarchy wh ON w.workload_id = wh.workload_id
				),
				lens_reviews_with_rank AS (
					SELECT *, 
						   ROW_NUMBER() OVER (PARTITION BY workload_id ORDER BY updated_at DESC) as rn
					FROM aws_wellarchitected_lens_review
				)
				SELECT wh.workload_name, lr.lens_name, lr.rn
				FROM workload_hierarchy wh
				JOIN lens_reviews_with_rank lr ON wh.workload_id = lr.workload_id
				WHERE lr.rn = 1
			`,
			expectedTables: []string{"aws_wellarchitected_lens_review", "aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
				"aws_wellarchitected_lens_review": {
					"wellarchitected:GetLensReview",
					"wellarchitected:GetWorkload",
					"wellarchitected:ListLensReviews",
					"wellarchitected:ListWorkloads",
				},
			},
		},
		{
			name:     "Access Analyzer table",
			sqlQuery: "SELECT name, status, type FROM aws_accessanalyzer_analyzer WHERE type = 'ACCOUNT'",
			expectedTables: []string{"aws_accessanalyzer_analyzer"},
			expectedOps: map[string][]string{
				"aws_accessanalyzer_analyzer": {
					"access-analyzer:GetAnalyzer",
					"access-analyzer:ListAnalyzers",
					"access-analyzer:ListFindings",
				},
			},
		},
		{
			name:     "Complex query with Access Analyzer and Well-Architected tables",
			sqlQuery: "SELECT a.name as analyzer_name, w.workload_name FROM aws_accessanalyzer_analyzer a CROSS JOIN aws_wellarchitected_workload w WHERE a.status = 'ACTIVE'",
			expectedTables: []string{"aws_accessanalyzer_analyzer", "aws_wellarchitected_workload"},
			expectedOps: map[string][]string{
				"aws_accessanalyzer_analyzer": {
					"access-analyzer:GetAnalyzer",
					"access-analyzer:ListAnalyzers",
					"access-analyzer:ListFindings",
				},
				"aws_wellarchitected_workload": {
					"wellarchitected:GetWorkload",
					"wellarchitected:ListWorkloads",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := AnalyzeSQLQuery(context.Background(), tt.sqlQuery)
			assert.NoError(t, err, "AnalyzeSQLQuery should not return an error")

			// Check that we got the expected number of tables
			assert.Len(t, result, len(tt.expectedOps), "Should return the expected number of tables")

			// Check each table's operations
			for tableName, expectedOps := range tt.expectedOps {
				actualOps, exists := result[tableName]
				assert.True(t, exists, "Table %s should be in the result", tableName)
				assert.ElementsMatch(t, expectedOps, actualOps, "Operations for table %s should match expected", tableName)
			}

			fmt.Printf("Test: %s\n", tt.name)
			fmt.Printf("SQL: %s\n", tt.sqlQuery)
			fmt.Printf("Result:\n")
			for table, ops := range result {
				fmt.Printf("  Table: %s\n", table)
				for _, op := range ops {
					fmt.Printf("    - %s\n", op)
				}
			}
			fmt.Println()
		})
	}
}

func TestAnalyzeExtractTableNames(t *testing.T) {
	tests := []struct {
		name     string
		sqlQuery string
		expected []string
	}{
		{
			name:     "Simple SELECT",
			sqlQuery: "SELECT * FROM aws_s3_bucket",
			expected: []string{"aws_s3_bucket"},
		},
		{
			name:     "Multiple tables with JOIN",
			sqlQuery: "SELECT * FROM aws_s3_bucket b JOIN aws_s3_object o ON b.name = o.bucket_name",
			expected: []string{"aws_s3_bucket", "aws_s3_object"},
		},
		{
			name:     "With schema prefix",
			sqlQuery: "SELECT * FROM aws.aws_ec2_instance",
			expected: []string{"aws_ec2_instance"},
		},
		{
			name:     "Mixed case",
			sqlQuery: "SELECT * FROM AWS_S3_BUCKET WHERE name = 'test'",
			expected: []string{"aws_s3_bucket"},
		},
		{
			name:     "Non-AWS table filtered out",
			sqlQuery: "SELECT * FROM aws_s3_bucket b JOIN other_table o ON b.id = o.id",
			expected: []string{"aws_s3_bucket"},
		},
		{
			name:     "INSERT statement",
			sqlQuery: "INSERT INTO aws_s3_bucket (name) VALUES ('test')",
			expected: []string{"aws_s3_bucket"},
		},
		{
			name:     "UPDATE statement",
			sqlQuery: "UPDATE aws_s3_bucket SET versioning = true WHERE name = 'test'",
			expected: []string{"aws_s3_bucket"},
		},
		{
			name:     "DELETE statement",
			sqlQuery: "DELETE FROM aws_s3_bucket WHERE name = 'test'",
			expected: []string{"aws_s3_bucket"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractTableNames(tt.sqlQuery)
			assert.NoError(t, err, "extractTableNames should not return an error")
			assert.ElementsMatch(t, tt.expected, result, "Extracted table names should match expected")
		})
	}
}

func TestAnalyzeGetAWSOperationsFromSQL(t *testing.T) {
	tests := []struct {
		name         string
		sqlQuery     string
		expectedOps  []string
	}{
		{
			name:     "Simple SELECT query",
			sqlQuery: "SELECT * FROM aws_wellarchitected_lens_review WHERE workload_id = 'test'",
			expectedOps: []string{
				"wellarchitected:GetLensReview",
				"wellarchitected:GetWorkload",
				"wellarchitected:ListLensReviews",
				"wellarchitected:ListWorkloads",
			},
		},
		{
			name:     "Query with JOIN - should deduplicate operations",
			sqlQuery: "SELECT w.workload_name, lr.lens_name FROM aws_wellarchitected_workload w JOIN aws_wellarchitected_lens_review lr ON w.workload_id = lr.workload_id",
			expectedOps: []string{
				"wellarchitected:GetLensReview",
				"wellarchitected:GetWorkload",
				"wellarchitected:ListLensReviews", 
				"wellarchitected:ListWorkloads",
			},
		},
		{
			name:     "Complex query with subquery",
			sqlQuery: "SELECT * FROM aws_wellarchitected_workload WHERE workload_id IN (SELECT workload_id FROM aws_wellarchitected_lens_review WHERE lens_name = 'test')",
			expectedOps: []string{
				"wellarchitected:GetLensReview",
				"wellarchitected:GetWorkload", 
				"wellarchitected:ListLensReviews",
				"wellarchitected:ListWorkloads",
			},
		},
		{
			name:     "PostgreSQL CTE",
			sqlQuery: "WITH workload_data AS (SELECT workload_id, workload_name FROM aws_wellarchitected_workload WHERE workload_id = 'test') SELECT lr.* FROM aws_wellarchitected_lens_review lr JOIN workload_data wd ON lr.workload_id = wd.workload_id",
			expectedOps: []string{
				"wellarchitected:GetLensReview",
				"wellarchitected:GetWorkload",
				"wellarchitected:ListLensReviews",
				"wellarchitected:ListWorkloads",
			},
		},
		{
			name:        "Query with unknown AWS table",
			sqlQuery:    "SELECT * FROM aws_unknown_table",
			expectedOps: []string{}, // Should return empty since table is not in mapping
		},
		{
			name:     "Access Analyzer table",
			sqlQuery: "SELECT name, status, type FROM aws_accessanalyzer_analyzer WHERE type = 'ACCOUNT'",
			expectedOps: []string{
				"access-analyzer:GetAnalyzer",
				"access-analyzer:ListAnalyzers",
				"access-analyzer:ListFindings",
			},
		},
		{
			name:     "Complex query with Access Analyzer and Well-Architected tables",
			sqlQuery: "SELECT a.name as analyzer_name, w.workload_name FROM aws_accessanalyzer_analyzer a CROSS JOIN aws_wellarchitected_workload w WHERE a.status = 'ACTIVE'",
			expectedOps: []string{
				"access-analyzer:GetAnalyzer",
				"access-analyzer:ListAnalyzers",
				"access-analyzer:ListFindings",
				"wellarchitected:GetWorkload",
				"wellarchitected:ListWorkloads",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GetAWSOperationsFromSQL(context.Background(), tt.sqlQuery)
			assert.NoError(t, err, "GetAWSOperationsFromSQL should not return an error")
			assert.ElementsMatch(t, tt.expectedOps, result, "Operations should match expected")

			fmt.Printf("Test: %s\n", tt.name)
			fmt.Printf("SQL: %s\n", tt.sqlQuery)
			fmt.Printf("AWS Operations:\n")
			for _, op := range result {
				fmt.Printf("  - %s\n", op)
			}
			fmt.Println()
		})
	}
}

func TestAnalyzeExampleGetAWSOperationsFromSQL(t *testing.T) {
	// Example: Analyze what AWS operations a complex query would trigger
	query := `
		WITH recent_workloads AS (
			SELECT workload_id, workload_name 
			FROM aws_wellarchitected_workload 
			WHERE created_date > '2024-01-01'
		)
		SELECT rw.workload_name, lr.lens_name, lr.risk_counts
		FROM recent_workloads rw
		JOIN aws_wellarchitected_lens_review lr ON rw.workload_id = lr.workload_id
		WHERE lr.lens_status = 'CURRENT'
	`

	operations, err := GetAWSOperationsFromSQL(context.Background(), query)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Println("AWS Operations that would be triggered:")
	for _, op := range operations {
		t.Logf("- %s\n", op)
	}
	
	// Output:
	// AWS Operations that would be triggered:
	// - wellarchitected:GetLensReview
	// - wellarchitected:GetWorkload
	// - wellarchitected:ListLensReviews
	// - wellarchitected:ListWorkloads
}


