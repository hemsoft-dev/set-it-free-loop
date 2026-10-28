import unittest
from check_v0_review import check

HEAD = 'a' * 40


class ReviewFormatTests(unittest.TestCase):
    def review(self):
        labels = ['Files reviewed', 'Context read', 'Verified', 'Not verified', 'Tests run']
        body = '### Key Findings\n\nNone.\n\n### Coverage\n\n'
        for label in labels:
            if label == 'Tests run':
                body += '- **Tests run:** None. Reviewer execution is disabled.\n'
            else:
                body += '- **' + label + ':**\n  - `file.py`\n  - Second item.\n'
        body += '\n\n---\n<!-- sfl request=123 sha=' + HEAD + ' -->'
        return {'commit_id': HEAD, 'user': {'id': 345646943, 'login': 'hemsoft-sfl-reviewer[bot]'}, 'body': body}

    def test_valid(self):
        self.assertEqual(check(self.review(), HEAD), [])

    def test_heading_text_in_inline_code_is_not_a_section(self):
        review = self.review()
        review['body'] = 'The change touches `### Coverage` and `### Key Findings`.\n' + review['body']
        self.assertEqual(check(review, HEAD), [])

    def test_duplicate_section_heading_is_rejected(self):
        review = self.review()
        review['body'] += '\n### Coverage\n'
        self.assertIn('expected one findings and coverage section', check(review, HEAD))

    def test_rejects_old_inline_coverage(self):
        review = self.review()
        review['body'] = review['body'].replace('**Files reviewed:**\n  - ', '**Files reviewed:** ')
        self.assertIn('Files reviewed must use nested coverage bullets', check(review, HEAD))

    def test_empty_optional_coverage(self):
        review = self.review()
        review['body'] = review['body'].replace(
            '**Context read:**\n  - `file.py`\n  - Second item.', '**Context read:** None reported.')
        self.assertEqual(check(review, HEAD), [])

    def test_required_coverage_cannot_be_empty(self):
        review = self.review()
        review['body'] = review['body'].replace(
            '**Verified:**\n  - `file.py`\n  - Second item.', '**Verified:** None reported.')
        self.assertIn('Verified must use nested coverage bullets', check(review, HEAD))

    def test_empty_group_cannot_hide_nested_content(self):
        review = self.review()
        review['body'] = review['body'].replace('**Context read:**', '**Context read:** None reported.')
        self.assertIn('Context read has content after None reported', check(review, HEAD))

    def test_wrong_identity_head_and_marker(self):
        review = self.review()
        review['user']['id'] = 1
        review['commit_id'] = 'b' * 40
        review['body'] = review['body'].replace(HEAD, 'b' * 40)
        self.assertEqual(len(check(review, HEAD)), 3)

    def test_entities_and_duplicate_disclaimer(self):
        for entity in ('&#x27;', '&\\#x27;'):
            review = self.review()
            review['body'] += '\nresponse' + entity + 's\nNo tests were executed.'
            self.assertEqual(len(check(review, HEAD)), 2)

    def test_literal_entity_examples_are_valid_source_evidence(self):
        for example in ('`&#x27;`', '``a`&#39;``', '`&\\#x27;`', '&amp;#x27;', '&amp;#39;'):
            with self.subTest(example=example):
                review = self.review()
                review['body'] = 'The source contains ' + example + '.\n' + review['body']
                self.assertEqual(check(review, HEAD), [])

    def test_literal_disclaimer_quotations_are_valid_source_evidence(self):
        for source in ('No tests were executed', '- **Tests run:** None. Reviewer execution is disabled.'):
            with self.subTest(source=source):
                review = self.review()
                review['body'] = 'The source contains `' + source + '`.\n' + review['body']
                self.assertEqual(check(review, HEAD), [])

    def test_discussing_legacy_disclaimer_in_prose_is_valid(self):
        review = self.review()
        review['body'] = 'The source checks for No tests were executed as its legacy text.\n' + review['body']
        self.assertEqual(check(review, HEAD), [])

    def test_quoted_marker_cannot_replace_the_parent_marker(self):
        review = self.review()
        marker = '<!-- sfl request=123 sha=' + HEAD + ' -->'
        review['body'] = 'The source contains `' + marker + '`.\n' + review['body'].replace(HEAD, 'b' * 40)
        self.assertIn('request marker does not name the expected head', check(review, HEAD))

    def test_every_coverage_item_must_stay_nested(self):
        for label in ('Files reviewed', 'Context read', 'Verified', 'Not verified'):
            with self.subTest(label=label):
                review = self.review()
                review['body'] = review['body'].replace(
                    '**' + label + ':**\n  - `file.py`\n  - Second item.',
                    '**' + label + ':**\n  - `file.py`\n- Second item.')
                self.assertIn(label + ' must use nested coverage bullets', check(review, HEAD))

    def test_tests_disclaimer_cannot_have_extra_coverage_items(self):
        review = self.review()
        review['body'] = review['body'].replace('\n\n---', '\n- Orphaned item.\n\n---')
        self.assertIn('unexpected content after tests disclaimer', check(review, HEAD))

    def test_disclaimer_elsewhere_cannot_validate_an_execution_claim(self):
        review = self.review()
        disclaimer = '- **Tests run:** None. Reviewer execution is disabled.'
        review['body'] = review['body'].replace(disclaimer, '- **Tests run:** pytest passed.')
        review['body'] += '\n' + disclaimer + '\n'
        self.assertIn('expected one parent-owned tests disclaimer', check(review, HEAD))

    def test_multiple_parent_markers_are_rejected(self):
        for head in (HEAD, 'b' * 40):
            review = self.review()
            review['body'] += '\n<!-- sfl request=456 sha=' + head + ' -->\n'
            self.assertIn('request marker does not name the expected head', check(review, HEAD))

    def test_bold_orphan_bullets_cannot_end_a_coverage_group(self):
        for orphan in ('- **Second item.**', '- **Unknown:** content'):
            with self.subTest(orphan=orphan):
                review = self.review()
                review['body'] = review['body'].replace('  - Second item.', orphan, 1)
                self.assertIn('Files reviewed must use nested coverage bullets', check(review, HEAD))
        review = self.review()
        review['body'] = review['body'].replace('### Coverage\n\n', '### Coverage\n\n- **Orphan**\n')
        self.assertIn('unexpected content before first coverage group', check(review, HEAD))

    def test_findings_section_cannot_follow_the_footer(self):
        review = self.review()
        review['body'] = review['body'].replace('### Key Findings\n\nNone.\n\n', '')
        review['body'] += '\n### Key Findings\nNone.\n'
        self.assertIn('findings must precede coverage', check(review, HEAD))

    def test_missing_or_reordered_coverage(self):
        review = self.review()
        review['body'] = review['body'].replace('**Files reviewed:**', '**Context read:**')
        self.assertTrue(check(review, HEAD))


if __name__ == '__main__':
    unittest.main()
