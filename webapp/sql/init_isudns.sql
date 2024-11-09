DROP INDEX idx_01 ON `records`;
ALTER TABLE `records` ADD INDEX idx_01 (name, disabled);
