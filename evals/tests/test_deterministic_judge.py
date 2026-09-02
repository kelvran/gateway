from evals.judge.deterministic import exact_match, regex_match


def test_exact_match_true_on_identical_strings():
    assert exact_match("hello", "hello") is True


def test_exact_match_false_on_different_strings():
    assert exact_match("hello", "Hello") is False


def test_exact_match_empty_strings():
    assert exact_match("", "") is True


def test_regex_match_true_when_pattern_found():
    assert regex_match("the answer is 42", r"\d+") is True


def test_regex_match_false_when_pattern_absent():
    assert regex_match("no numbers here", r"\d+") is False


def test_regex_match_anchored_pattern():
    assert regex_match("42 is the answer", r"^\d+") is True
    assert regex_match("the answer is 42", r"^\d+") is False


def test_regex_match_special_characters_in_pattern():
    assert regex_match("cost: $5.00", r"\$\d+\.\d{2}") is True
