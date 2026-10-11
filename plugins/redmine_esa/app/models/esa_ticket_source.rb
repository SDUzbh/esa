class EsaTicketSource < ApplicationRecord
  belongs_to :issue
  belongs_to :esa_upload, optional: true
end
