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
        for entity in ('&#x27;', '&\\#x27;', '&amp;#39;'):
            review = self.review()
            review['body'] += '\nresponse' + entity + 's\nNo tests were executed.'
            self.assertEqual(len(check(review, HEAD)), 2)

    def test_missing_or_reordered_coverage(self):
        review = self.review()
        review['body'] = review['body'].replace('**Files reviewed:**', '**Context read:**')
        self.assertTrue(check(review, HEAD))


if __name__ == '__main__':
    unittest.main()
