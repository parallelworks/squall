# Changelog

## [0.4.0](https://github.com/parallelworks/squall/compare/v0.3.0...v0.4.0) (2026-10-18)


### Features

* EC2 launches take metadata, private DNS name, CPU and network card options ([#8](https://github.com/parallelworks/squall/issues/8)) ([01bcf71](https://github.com/parallelworks/squall/commit/01bcf7133e4d58edc5ae0ccc7bc16b1305714eed))

## [0.3.0](https://github.com/parallelworks/squall/compare/v0.2.0...v0.3.0) (2026-10-18)


### Features

* Amazon EC2 instances, images, placement groups and volumes ([#6](https://github.com/parallelworks/squall/issues/6)) ([68d9083](https://github.com/parallelworks/squall/commit/68d908310be1a7a53c0cd03af7af15abb4fa6e4d))

## [0.2.0](https://github.com/parallelworks/squall/compare/v0.1.0...v0.2.0) (2026-10-14)


### Features

* Amazon EC2 Elastic IP addresses and network interfaces ([#4](https://github.com/parallelworks/squall/issues/4)) ([87db7bb](https://github.com/parallelworks/squall/commit/87db7bb547ed7c8ca60e968a36e70855f72bedb0))

## 0.1.0 (2026-10-13)


### Features

* cloud API stand-ins with fault injection, starting with Compute Engine addresses ([7f46eb4](https://github.com/parallelworks/squall/commit/7f46eb4cd59773f8c8e2d065fdfc16a4fa5eb299))


### Bug fixes

* faults reach the caller as documented, retries with a requestId replay, and the module requires nothing ([#3](https://github.com/parallelworks/squall/issues/3)) ([6defd7c](https://github.com/parallelworks/squall/commit/6defd7c931a80ac27226c947ca0004aaba1cd431))


### CI

* lint and check vulnerabilities on the latest Go, test on the oldest and latest ([#2](https://github.com/parallelworks/squall/issues/2)) ([36f06bb](https://github.com/parallelworks/squall/commit/36f06bb7fe486276ff5126c9975ce424b48a4ada))
