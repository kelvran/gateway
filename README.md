# Repository Coverage

[Full report](https://htmlpreview.github.io/?https://github.com/kelvran/gateway/blob/python-coverage-comment-action-data/htmlcov/index.html)

| Name                             |    Stmts |     Miss |   Cover |   Missing |
|--------------------------------- | -------: | -------: | ------: | --------: |
| evals/\_\_init\_\_.py            |        1 |        0 |    100% |           |
| evals/audit\_corpus.py           |       32 |        0 |    100% |           |
| evals/auto\_flag.py              |       30 |        0 |    100% |           |
| evals/cli.py                     |      828 |       41 |     95% |238, 290, 294, 298, 322, 372, 447, 562-563, 641, 684, 755-778, 801-802, 853-869, 874-875, 1008, 1516, 1909-1910, 1913, 2122, 2196-2197, 2200, 2215-2216, 3206, 3280 |
| evals/corpus\_staleness.py       |       72 |        8 |     89% |155-156, 173-174, 179, 211, 217, 223 |
| evals/field\_swap\_lint.py       |       17 |        1 |     94% |        77 |
| evals/ingestion/\_\_init\_\_.py  |        0 |        0 |    100% |           |
| evals/ingestion/decode.py        |        5 |        0 |    100% |           |
| evals/ingestion/mapping.py       |       10 |        0 |    100% |           |
| evals/ingestion/object\_store.py |       52 |        0 |    100% |           |
| evals/judge/\_\_init\_\_.py      |        0 |        0 |    100% |           |
| evals/judge/cache.py             |       11 |        0 |    100% |           |
| evals/judge/deterministic.py     |        6 |        0 |    100% |           |
| evals/judge/llm\_judge.py        |      123 |        3 |     98% |632, 643-648 |
| evals/judge/providers.py         |      100 |        2 |     98% |   370-371 |
| evals/models.py                  |      104 |        0 |    100% |           |
| evals/online/\_\_init\_\_.py     |        0 |        0 |    100% |           |
| evals/online/sampler.py          |       22 |        0 |    100% |           |
| evals/results\_store.py          |       29 |        0 |    100% |           |
| evals/rollout/\_\_init\_\_.py    |        0 |        0 |    100% |           |
| evals/rollout/cache.py           |        7 |        0 |    100% |           |
| evals/rollout/sandbox.py         |       59 |       10 |     83% |118-122, 131-138, 200-201, 302 |
| evals/rollout/scheduler.py       |       69 |        0 |    100% |           |
| evals/stats.py                   |      124 |        4 |     97% |383, 448, 450, 455 |
| evals/tracing.py                 |       32 |        0 |    100% |           |
| evals/trend\_alert.py            |       63 |        1 |     98% |       191 |
| evals/webhook.py                 |       48 |        2 |     96% |   152-153 |
| **TOTAL**                        | **1844** |   **72** | **96%** |           |


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