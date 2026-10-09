-- 別 DB に同名の Parent があり、インデックスが多いケース
CREATE TABLE Parent (
  OrgID STRING(36) NOT NULL,
  ParentID STRING(36) NOT NULL,
  Name STRING(MAX) NOT NULL,
  Status INT64 NOT NULL,
) PRIMARY KEY(OrgID, ParentID);

CREATE INDEX ParentByName ON Parent(OrgID, Name);
CREATE INDEX ParentByStatus ON Parent(OrgID, Status);
