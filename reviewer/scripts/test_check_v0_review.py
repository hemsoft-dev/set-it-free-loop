import unittest
from check_v0_review import check

HEAD = 'a' * 40


class ReviewFormatTests(unittest.TestCase):
    def review(self):
        labels = ['Files reviewed', 'Context read', 'Verified', 'Not verified', 'Tests run']
        body = '### Key Findings\n\nNone.\n\n### Coverage\n\n'
        body += '\n'.join('- **' + label + ':** ' + (
            'None. Reviewer execution is disabled.' if label == 'Tests run' else '`file.py`'
        ) for label in labels)
        body += '\n\n---\n<!-- sfl request=123 sha=' + HEAD + ' -->'
        return {'commit_id': HEAD, 'user': {'id': 345646943, 'login': 'hemsoft-sfl-reviewer[bot]'}, 'body': body}

    def test_valid(self):
        self.assertEqual(check(self.review(), HEAD), [])

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
