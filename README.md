# Repository Coverage

[Full report](https://htmlpreview.github.io/?https://github.com/kelvran/gateway/blob/python-coverage-comment-action-data/htmlcov/index.html)

| Name                             |    Stmts |     Miss |   Cover |   Missing |
|--------------------------------- | -------: | -------: | ------: | --------: |
| evals/\_\_init\_\_.py            |        1 |        0 |    100% |           |
| evals/audit\_corpus.py           |       32 |        0 |    100% |           |
| evals/auto\_flag.py              |       30 |        0 |    100% |           |
| evals/cli.py                     |      792 |       42 |     95% |176, 198, 202, 206, 212, 230, 280, 355, 470-471, 549, 592, 663-686, 709-710, 761-777, 782-783, 900, 1381, 1668-1669, 1672, 1868, 1947-1948, 1951, 1965-1966, 2931, 3003 |
| evals/corpus\_staleness.py       |       72 |        8 |     89% |155-156, 173-174, 179, 211, 217, 223 |
| evals/field\_swap\_lint.py       |       17 |        1 |     94% |        77 |
| evals/ingestion/\_\_init\_\_.py  |        0 |        0 |    100% |           |
| evals/ingestion/decode.py        |        5 |        0 |    100% |           |
| evals/ingestion/mapping.py       |       10 |        0 |    100% |           |
| evals/ingestion/object\_store.py |       49 |        0 |    100% |           |
| evals/judge/\_\_init\_\_.py      |        0 |        0 |    100% |           |
| evals/judge/cache.py             |       11 |        0 |    100% |           |
| evals/judge/deterministic.py     |        6 |        0 |    100% |           |
| evals/judge/llm\_judge.py        |      100 |        3 |     97% |464, 474-479 |
| evals/judge/providers.py         |      100 |        2 |     98% |   370-371 |
| evals/models.py                  |      104 |        0 |    100% |           |
| evals/results\_store.py          |       29 |        0 |    100% |           |
| evals/rollout/\_\_init\_\_.py    |        0 |        0 |    100% |           |
| evals/rollout/cache.py           |        7 |        0 |    100% |           |
| evals/rollout/sandbox.py         |       51 |       26 |     49% |98-102, 111-118, 170-208, 217 |
| evals/rollout/scheduler.py       |       69 |        0 |    100% |           |
| evals/stats.py                   |       73 |        0 |    100% |           |
| evals/tracing.py                 |       32 |        0 |    100% |           |
| evals/trend\_alert.py            |       63 |        1 |     98% |       191 |
| **TOTAL**                        | **1653** |   **83** | **95%** |           |


## Setup coverage badge

Below are examples of the badges you can use in your main branch `README` file.

### Direct image

[![Coverage badge](https://raw.githubusercontent.com/kelvran/gateway/python-coverage-comment-action-data/badge.svg)](https://htmlpreview.github.io/?https://github.com/kelvran/gateway/blob/python-coverage-comment-action-data/htmlcov/index.html)

This is the one to use if your repository is private or if you don't want to customize anything.

### [Shields.io](https://shields.io) Json Endpoint

[![Coverage badge](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/kelvran/gateway/python-coverage-comment-action-data/endpoint.json)](https://htmlpreview.github.io/?https://github.com/kelvran/gateway/blob/python-coverage-comment-action-data/htmlcov/index.html)

Using this one will allow you to [customize](https://shields.io/endpoint) the look of your badge.
It won't work with private repositories. It won't be refreshed more than once per five minutes.

### [Shields.io](https://shields.io) Dynamic Badge

[![Coverage badge](https://img.shields.io/badge/dynamic/json?color=brightgreen&label=coverage&query=%24.message&url=https%3A%2F%2Fraw.githubusercontent.com%2Fkelvran%2Fgateway%2Fpython-coverage-comment-action-data%2Fendpoint.json)](https://htmlpreview.github.io/?https://github.com/kelvran/gateway/blob/python-coverage-comment-action-data/htmlcov/index.html)

This one will always be the same color. It won't work for private repos. I'm not even sure why we included it.

## What is that?

This branch is part of the
[python-coverage-comment-action](https://github.com/marketplace/actions/python-coverage-comment)
GitHub Action. All the files in this branch are automatically generated and may be
overwritten at any moment.