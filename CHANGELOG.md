# Changelog

## [0.9.0](https://github.com/parallelworks/squall/compare/v0.8.0...v0.9.0) (2026-10-23)


### Features

* **access:** permission policies that refuse calls the caller lacks permission for ([#28](https://github.com/parallelworks/squall/issues/28)) ([0ecd8c7](https://github.com/parallelworks/squall/commit/0ecd8c752165b3cefa19518469ab976725fb3f84))


### Bug fixes

* **deps:** build with Go 1.27.3, which distrusts eMudhra's compromised intermediate CAs ([#26](https://github.com/parallelworks/squall/issues/26)) ([ae19548](https://github.com/parallelworks/squall/commit/ae19548aa3e123c140c690276fc6ff0d4bd2edf1))

## [0.8.0](https://github.com/parallelworks/squall/compare/v0.7.0...v0.8.0) (2026-10-21)


### Features

* Amazon S3 and Cloud Storage buckets ([#24](https://github.com/parallelworks/squall/issues/24)) ([4e7f106](https://github.com/parallelworks/squall/commit/4e7f106d47d9cb6fcff7195a33635e1da677c788))

## [0.7.0](https://github.com/parallelworks/squall/compare/v0.6.1...v0.7.0) (2026-10-20)


### Features

* Amazon EC2 snapshots and image registration, and Compute Engine snapshots ([#16](https://github.com/parallelworks/squall/issues/16)) ([2a218f1](https://github.com/parallelworks/squall/commit/2a218f1183321132c58271182740dc43d0216ec9))

## [0.6.1](https://github.com/parallelworks/squall/compare/v0.6.0...v0.6.1) (2026-10-19)


### Bug fixes

* a scale set created without a capacity starts with no VMs instead of failing ([#14](https://github.com/parallelworks/squall/issues/14)) ([050ca0a](https://github.com/parallelworks/squall/commit/050ca0a136871ce81b0e329bc80a7972812bdbc4))

## [0.6.0](https://github.com/parallelworks/squall/compare/v0.5.0...v0.6.0) (2026-10-19)


### Features

* Azure Resource Manager resource groups, virtual machines, scale sets, networking and private DNS ([#12](https://github.com/parallelworks/squall/issues/12)) ([5239d68](https://github.com/parallelworks/squall/commit/5239d68dc9af356bb5bc6af20a76f856dfa89051))

## [0.5.0](https://github.com/parallelworks/squall/compare/v0.4.0...v0.5.0) (2026-10-19)


### Features

* Compute Engine instances, zones, disks and zonal operations ([#10](https://github.com/parallelworks/squall/issues/10)) ([96be1d5](https://github.com/parallelworks/squall/commit/96be1d5aa268bee9cd83eb1155faa72b9b134252))

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
