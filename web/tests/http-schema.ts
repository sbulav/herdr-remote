import Ajv2020 from 'ajv/dist/2020';
import addFormats from 'ajv-formats';
import schema from '../../protocol/http-v1.schema.json';

const ajv = new Ajv2020({ strict: true });
addFormats(ajv);
ajv.addKeyword({ keyword: 'x-endpoints', schemaType: 'object' });
ajv.addSchema(schema);

export function httpValidator(reference: string) {
  const validate = ajv.getSchema(`${schema.$id}${reference}`);
  if (!validate) throw new Error(`Missing HTTP schema ${reference}`);
  return validate;
}

export { schema };
